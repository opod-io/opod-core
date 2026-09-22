package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A worker loads its own model.
//
// When a manager starts a worker it names the model that worker exists to
// serve (OPOD_LOAD_MODEL, with OPOD_LOAD_REPO / OPOD_LOAD_FILE overriding the
// catalog). Until 2026-09-21 the container entrypoint acted on that by
// POSTing the worker's OWN /v1/model/load over HTTP with
// `Authorization: Bearer $OPOD_JOIN_TOKEN` — which made it the one caller of
// that API that could not sign HMAC, because a shell cannot. So
// OPOD_REJECT_BEARER=1, the switch that exists to close the transition path,
// could never be turned on: the worker registered and then refused its own
// load five times over ("unauthorized (HMAC required; bearer disabled)",
// measured on the design-partner cell). The load is the worker's own work, so
// it happens in the worker: no HTTP call, no token in an argument list, and
// nothing left that authenticates with a bearer.

// selfLoadWait is how long the worker waits for its engine to answer before
// the first load attempt, and how often it asks. The engine in a worker
// container is a sibling process that may still be starting (vLLM binds its
// port only after loading weights, which is minutes) — same bound the
// entrypoint's /healthz poll used.
var (
	selfLoadWait     = 2 * time.Minute
	selfLoadPoll     = 2 * time.Second
	selfLoadAttempts = 5
	selfLoadRetry    = 10 * time.Second
)

func (s *Server) logger() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// LoadBody decides how this worker asks for a model it was given a repo and a
// file for: by PATH when the whole file is already at the top of the node's
// model cache, by repo/file otherwise.
//
// A PINNED model (ModelRevision and/or ModelSHA256) never goes by path: a file
// that merely has the same NAME is not the version that was asked for, and
// loading it by path skips both the revision directory and the digest check —
// a node that had ever served the model unpinned then served those bytes under
// every pinned version. The fetch puts a pinned file in <models>/<repo>@<rev>/
// and verifies it there.
func (s *Server) LoadBody(id, repo, file string) LoadRequest {
	if s.ModelRevision == "" && s.ModelSHA256 == "" && file != "" && s.ModelsDir != "" {
		if st, err := os.Stat(filepath.Join(s.ModelsDir, file)); err == nil && !st.IsDir() {
			return LoadRequest{ID: id, Path: filepath.Join(s.ModelsDir, file)}
		}
	}
	return LoadRequest{ID: id, Repo: repo, File: file}
}

// SelfLoad makes the model this worker was started for resident, waiting for
// the engine first and retrying a failed load. It logs what happened and never
// fails the process: a worker whose load does not take is a worker the leader
// sees with no model — which is the state the manager can act on — while a
// worker that exits takes its registration and its heartbeat with it.
func (s *Server) SelfLoad(ctx context.Context, req LoadRequest) {
	log := s.logger()
	if req.ID == "" {
		return
	}
	if launchedByLoad(s.Engine.Name()) {
		// The load is what STARTS this engine, so waiting for it to be healthy
		// first can only ever time out: measured on the design-partner cell,
		// where every llama.cpp worker sat through the full wait and then
		// loaded successfully ("engine did not answer, loading anyway"), two
		// minutes later than it needed to serve. The retry loop below is what
		// covers an engine that is not ready.
		s.attemptLoad(ctx, req, log)
		return
	}
	if !s.waitForEngine(ctx) {
		if ctx.Err() != nil {
			return // the worker is stopping: not a load, and not a complaint
		}
		// The wait ran out. Try anyway: an engine can serve without answering
		// the health check this driver happens to use, and a load is how we
		// find out.
		log.Warn("self-load: engine did not answer, loading anyway", "model", req.ID, "waited", selfLoadWait)
	}
	s.attemptLoad(ctx, req, log)
}

// attemptLoad is the retry loop: the load, then up to selfLoadAttempts of it.
func (s *Server) attemptLoad(ctx context.Context, req LoadRequest, log *slog.Logger) {
	for attempt := 1; attempt <= selfLoadAttempts; attempt++ {
		name, err := s.LoadModel(ctx, req)
		if err == nil {
			log.Info("self-load: model resident", "model", req.ID, "engine_name", name, "attempt", attempt)
			return
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("self-load failed", "model", req.ID, "attempt", attempt, "of", selfLoadAttempts, "err", err)
		if attempt == selfLoadAttempts {
			log.Error("self-load gave up; this worker registers with no model", "model", req.ID)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(selfLoadRetry):
		}
	}
}

// launchedByLoad says whether a load STARTS this engine's process. vLLM,
// SGLang and llama.cpp have no persistent server of their own — the model is
// chosen at launch, which is why the load handler launches them — so such an
// engine is unhealthy until the first load and a worker must not wait for it.
func launchedByLoad(engine string) bool {
	for _, p := range []string{"vllm", "sglang", "llamacpp", "llama-cpp"} {
		if strings.HasPrefix(engine, p) {
			return true
		}
	}
	return false
}

// waitForEngine returns true once the engine answers its health check.
func (s *Server) waitForEngine(ctx context.Context) bool {
	deadline := time.Now().Add(selfLoadWait)
	for {
		if err := s.Engine.Health(ctx); err == nil {
			return true
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(selfLoadPoll):
		}
	}
}
