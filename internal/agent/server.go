// Worker HTTP server. Started by `opod join` so the leader can reach this
// node's local inference engine via the mesh. Bound to the agent's tailnet /
// LAN address — not 0.0.0.0 — so only mesh members can connect.
//
// The exposed surface is OpenAI-compatible passthrough to the local engine,
// authenticated with the worker token (the same secret the leader uses for
// outbound calls to this worker). The leader's RoutingEngine talks to this
// endpoint exactly the way it would talk to a standalone vLLM/MLX server.
package agent

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
)

// Server is the worker's HTTP surface.
type Server struct {
	Engine     engines.Engine
	Token      string // shared secret; same value the leader stores in node.worker_token
	Supervisor *Supervisor
	// ModelsDir is the writable root for GGUFs uploaded by the leader's
	// sharding orchestrator (see /v1/process/upload). If empty, upload is
	// refused with 503. Set by `opod join` from cfg.Storage.ModelsDir.
	ModelsDir string

	http *http.Server
}

// Start runs the server until ctx is done. Returns the listen error or nil
// on graceful shutdown.
func (s *Server) Start(ctx context.Context, listen string) error {
	if s.Supervisor == nil {
		s.Supervisor = NewSupervisor(nil)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/v1/models", s.auth(s.listModels))
	mux.HandleFunc("/v1/chat/completions", s.auth(s.chatCompletions))
	mux.HandleFunc("/v1/model/load", s.auth(s.modelLoad)) // leader-side placement: pull+load a model on this worker
	mux.HandleFunc("/v1/process/start", s.auth(s.processStart))
	mux.HandleFunc("/v1/process/stop", s.auth(s.processStop))
	mux.HandleFunc("/v1/process/list", s.auth(s.processList))
	mux.HandleFunc("/v1/process/get", s.auth(s.processGet)) // readiness poll for the 202 from /start
	mux.HandleFunc("/v1/process/logs", s.auth(s.processLogs))
	mux.HandleFunc("/v1/process/file", s.auth(s.fileCheck))    // HEAD: does this GGUF exist with matching sha?
	mux.HandleFunc("/v1/process/upload", s.auth(s.fileUpload)) // POST: stream a GGUF up

	s.http = &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 30 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- s.http.ListenAndServe() }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return s.http.Shutdown(shutdownCtx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("worker listen: %w", err)
		}
		return nil
	}
}

// auth gates every worker endpoint. Accepts either:
//
//	X-Opod-Auth: v=1,id=...,ts=...,sig=...   (preferred — HMAC, token never travels)
//	Authorization: Bearer <token>             (transition mode — kept for one release)
//
// HMAC is tried first when the header is present. If verification fails, the
// request is rejected without falling through to bearer (catches active
// attempts to downgrade). When only Authorization is present, we accept the
// bearer for backwards compat — the user can disable that path by setting
// OPOD_REJECT_BEARER=1 in the worker environment once they've confirmed
// every leader is signing.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token == "" {
			http.Error(w, "worker has no token configured", http.StatusUnauthorized)
			return
		}
		if r.Header.Get(auth.HMACHeader) != "" {
			// Worker only knows its own token, so the lookup ignores nodeID.
			if _, err := auth.VerifyRequest(r, func(string) (string, error) {
				return s.Token, nil
			}); err != nil {
				http.Error(w, "unauthorized (hmac): "+err.Error(), http.StatusUnauthorized)
				return
			}
			next(w, r)
			return
		}
		if os.Getenv("OPOD_REJECT_BEARER") == "1" {
			http.Error(w, "unauthorized (HMAC required; bearer disabled)", http.StatusUnauthorized)
			return
		}
		// Bearer fallback.
		got := r.Header.Get("Authorization")
		if strings.HasPrefix(got, "Bearer ") {
			got = strings.TrimPrefix(got, "Bearer ")
		}
		// Constant-time compare so response timing can't leak how much of
		// the token an attacker has guessed. s.Token is non-empty here (the
		// empty case 401s at the top), so the length-leak on mismatched
		// sizes reveals nothing useful.
		if subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	if err := s.Engine.Health(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "engine: %v", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	models, err := s.Engine.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type obj struct {
		ID     string `json:"id"`
		Object string `json:"object"`
	}
	type list struct {
		Object string `json:"object"`
		Data   []obj  `json:"data"`
	}
	out := list{Object: "list"}
	for _, m := range models {
		out.Data = append(out.Data, obj{ID: m, Object: "model"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// chatCompletions accepts an OpenAI-format chat request and proxies it to the
// local engine. Streaming and non-streaming both supported (the engine's Chat
// returns a channel either way; we re-emit it as SSE for stream=true).
func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Model       string              `json:"model"`
		Messages    []map[string]string `json:"messages"`
		System      string              `json:"system,omitempty"`
		Stream      bool                `json:"stream,omitempty"`
		Temperature *float32            `json:"temperature,omitempty"`
		TopP        *float32            `json:"top_p,omitempty"`
		MaxTokens   *int                `json:"max_tokens,omitempty"`
		Stop        []string            `json:"stop,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages required", http.StatusBadRequest)
		return
	}
	msgs := make([]engines.Message, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, engines.Message{Role: m["role"], Content: m["content"]})
	}
	engReq := engines.ChatRequest{
		Model:       req.Model,
		System:      req.System,
		Messages:    msgs,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		Stream:      true,
	}
	stream, err := s.Engine.Chat(r.Context(), engReq)
	if err != nil {
		http.Error(w, "engine: "+err.Error(), http.StatusBadGateway)
		return
	}

	if req.Stream {
		writeSSE(w, r, stream, req.Model)
	} else {
		writeAggregate(w, stream, req.Model)
	}
}

func writeSSE(w http.ResponseWriter, r *http.Request, stream <-chan engines.StreamEvent, model string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	id := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	sendChunk := func(payload map[string]any) {
		b, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "data: %s\n\n", string(b))
		if flusher != nil {
			flusher.Flush()
		}
	}

	// initial role chunk
	sendChunk(map[string]any{
		"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
		"choices": []map[string]any{{
			"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil,
		}},
	})

	defer func() {
		// drain in background so engine producer never blocks
		go func() {
			for range stream {
			}
		}()
	}()

	for ev := range stream {
		if r.Context().Err() != nil {
			return
		}
		if ev.Err != nil {
			sendChunk(map[string]any{"error": map[string]any{"message": ev.Err.Error()}})
			return
		}
		if ev.Done {
			reason := ev.Reason
			if reason == "" {
				reason = "stop"
			}
			final := map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []map[string]any{{
					"index": 0, "delta": map[string]any{}, "finish_reason": reason,
				}},
			}
			if ev.Usage != nil {
				final["usage"] = map[string]int{
					"prompt_tokens":     ev.Usage.PromptTokens,
					"completion_tokens": ev.Usage.CompletionTokens,
					"total_tokens":      ev.Usage.TotalTokens,
				}
			}
			sendChunk(final)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		if ev.Delta != "" {
			sendChunk(map[string]any{
				"id": id, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []map[string]any{{
					"index": 0, "delta": map[string]any{"content": ev.Delta}, "finish_reason": nil,
				}},
			})
		}
	}
}

// modelLoad makes THIS worker pull a model into its own engine and warm-load
// it (when the engine supports warm loading). It's the leader-side placement
// primitive: the leader's orchestrator POSTs here so a non-sharded model can be
// pinned to a specific worker. Once resident, the worker reports the model in
// its next heartbeat and the leader reconciles the placement — so this endpoint
// only needs to make the model resident, not touch any store.
//
// The leader sends the model's catalog source fields (not a pre-resolved name)
// so the worker resolves the engine-native name against ITS OWN engine — correct
// for a heterogeneous fleet where the worker's engine differs from the leader's.
// Primitive fields (rather than models.Entry) keep this package free of an
// import cycle (models imports agent).
func (s *Server) modelLoad(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		ID         string `json:"id"`
		OllamaName string `json:"ollama_name"`
		Repo       string `json:"repo"`
		Path       string `json:"path"`
		Pin        bool   `json:"pin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.ID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	name := engines.NativeName(s.Engine.Name(), engines.Source{
		ID: req.ID, OllamaName: req.OllamaName, Repo: req.Repo, Path: req.Path,
	})

	// Pull weights (may take minutes; for llama-server/vLLM the driver Pull is
	// a no-op and the model is fetched at engine launch / load).
	if err := s.Engine.Pull(r.Context(), name, nil); err != nil {
		http.Error(w, "pull: "+err.Error(), http.StatusBadGateway)
		return
	}
	// vLLM has NO persistent server and its driver never starts one — the model
	// only becomes resident once `vllm serve <model>` is running. Launch it here
	// under the supervisor so leader-side placement actually serves. Fire-and-
	// forget (no readiness gate): vLLM binds its port only after a multi-minute
	// model load, so we let it warm up in the background — requests 503 until
	// ready, then the heartbeat reports the model and the leader reconciles.
	if strings.HasPrefix(s.Engine.Name(), "vllm") {
		if err := s.launchVLLM(name, req.ID); err != nil {
			http.Error(w, "vllm serve: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	// llama.cpp whole-model placement (for load-balancing replicas): launch a
	// whole llama-server for this model. Like vLLM, the driver never starts one
	// on its own for a non-sharded placement (only the shard orchestrator does),
	// so `model add --node a,b,c` would otherwise leave nothing serving.
	if strings.HasPrefix(s.Engine.Name(), "llamacpp") || strings.HasPrefix(s.Engine.Name(), "llama-cpp") {
		if err := s.launchLlamaServer(name, req.Repo, req.Path, req.ID); err != nil {
			http.Error(w, "llama-server: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	// Warm-load into memory when the engine can (ollama). Engines that can't
	// (vLLM/MLX/llama-server) don't implement Loader — the Pull above is enough
	// and they become resident on first use; treat that as success.
	if l, ok := s.Engine.(engines.Loader); ok {
		if err := l.Load(r.Context(), name, req.Pin); err != nil && !errors.Is(err, engines.ErrUnloadNotSupported) {
			http.Error(w, "load: "+err.Error(), http.StatusBadGateway)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "model": name})
}

// launchVLLM (re)starts `vllm serve <model>` on the host:port the vLLM driver
// probes, replacing any model already being served (exclusive placement). It
// does NOT wait for readiness: vLLM binds its port only after a lengthy model
// load, so gating here would time out — the supervisor starts it in the
// background (restarting on crash) and the model becomes servable when vLLM
// finishes warming up, observed via the engine's /v1/models on the next heartbeat.
func (s *Server) launchVLLM(model, servedName string) error {
	host, port := "127.0.0.1", 8000
	if ep, ok := s.Engine.(interface{ Endpoint() string }); ok {
		if u, err := url.Parse(ep.Endpoint()); err == nil {
			if h := u.Hostname(); h != "" {
				host = h
			}
			if p := u.Port(); p != "" {
				if n, err := strconv.Atoi(p); err == nil {
					port = n
				}
			}
		}
	}
	_ = s.Supervisor.Stop("vllm-serve") // exclusive: one model per worker
	// Launch via a login shell with `exec`, NOT a bare exec.Command: vLLM's
	// multiprocessing worker-spawn hangs before loading when launched directly by
	// the (multithreaded, new-process-group, null-stdin) supervisor, but starts
	// fine from a shell — so mirror the shell invocation and let exec replace sh
	// with vllm so the supervisor still monitors the real PID. Flags:
	//   --trust-remote-code       many HF repos (MiMo, some Qwen) ship custom code
	//   --gpu-memory-utilization  0.90 default OOMs on KV-cache right after loading
	//                             weights (grabs ~full VRAM then exits); 0.85 leaves headroom
	// VLLM_WORKER_MULTIPROC_METHOD=spawn avoids fork-in-multithreaded-parent deadlocks.
	// --served-model-name <catalog id>: vLLM otherwise serves under the HF repo
	// name; serving under the catalog id means the model routes end-to-end by id
	// (heartbeat placement, router, and this worker's proxy all agree) with no
	// name translation. Falls back to the repo name when the id is empty.
	if servedName == "" {
		servedName = model
	}
	// --tensor-parallel-size: split the model across ALL GPUs on this box (TP
	// belongs inside one machine — a fat NVLink/PCIe link). Auto-detect the count
	// (NVIDIA via nvidia-smi, else /dev/dri render nodes for AMD/Intel) so a
	// multi-GPU worker uses every GPU instead of vLLM's default of 1. Needs the
	// container launched with a large --shm-size for the per-GPU
	// worker processes' shared memory.
	// vLLM requires tensor-parallel-size to DIVIDE the model's attention-head
	// count (almost always a power of 2), and to be <= the GPU count. So pick the
	// largest power of 2 <= detected GPUs (5 GPUs -> TP=4, not 5 which would fail
	// "heads (32) must be divisible by 5"). Detect via nvidia-smi, else /dev/dri.
	// --gpu-memory-utilization: 0.85 by default. When the worker was given a
	// per-worker VRAM budget (OPOD_VRAM_BUDGET_GB, set by a control plane or by
	// hand — `opod join --vram-budget`), the fraction is budget / device VRAM
	// (clamped 0.05–0.95) so several workers can share one device under a
	// ledger instead of each grabbing 85% of it. Budget is only honoured when
	// nvidia-smi can report the device size; otherwise the default stands.
	cmdline := fmt.Sprintf(
		"N=$(nvidia-smi -L 2>/dev/null | grep -c GPU); "+
			"[ \"$N\" -ge 1 ] || N=$(ls /dev/dri/renderD* 2>/dev/null | wc -l); "+
			"[ \"$N\" -ge 1 ] || N=1; "+
			"TP=1; while [ $((TP*2)) -le \"$N\" ]; do TP=$((TP*2)); done; "+
			"U=0.85; B=\"${OPOD_VRAM_BUDGET_GB:-0}\"; "+
			"if [ \"$B\" -gt 0 ] 2>/dev/null; then T=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null | head -1 | tr -d ' '); "+
			"[ -n \"$T\" ] && U=$(awk -v b=\"$B\" -v t=\"$T\" 'BEGIN{u=b*1024/t; if(u>0.95)u=0.95; if(u<0.05)u=0.05; printf \"%%.2f\", u}'); fi; "+
			"exec vllm serve '%s' --served-model-name '%s' '%s' --host %s --port %d "+
			"--trust-remote-code --gpu-memory-utilization \"$U\" --tensor-parallel-size \"$TP\"",
		model, servedName, model, host, port)
	_, err := s.Supervisor.Start(context.Background(), ProcessSpec{
		ID:          "vllm-serve",
		Command:     "/bin/sh",
		Args:        []string{"-lc", cmdline},
		Env:         map[string]string{"VLLM_WORKER_MULTIPROC_METHOD": "spawn"},
		Restart:     true,
		MaxRestarts: 3,
	})
	return err
}

// launchLlamaServer (re)starts a whole `llama-server` for a non-sharded llama.cpp
// placement, on the host:port the llama.cpp driver probes. Exclusive (one model
// per worker), fire-and-forget (llama-server binds its port only after a possibly
// multi-GB HF pull, so a readiness gate would time out). --alias makes it serve
// under the catalog id so the model routes end-to-end by id with no translation.
func (s *Server) launchLlamaServer(nativeName, repo, path, alias string) error {
	host, port := "127.0.0.1", 8080
	if ep, ok := s.Engine.(interface{ Endpoint() string }); ok {
		if u, err := url.Parse(ep.Endpoint()); err == nil {
			if h := u.Hostname(); h != "" {
				host = h
			}
			if p := u.Port(); p != "" {
				if n, err := strconv.Atoi(p); err == nil {
					port = n
				}
			}
		}
	}
	var src []string
	switch {
	case repo != "":
		src = []string{"-hf", repo}
	case path != "":
		src = []string{"-m", path}
	default:
		src = []string{"-hf", nativeName}
	}
	if alias == "" {
		alias = nativeName
	}
	args := append(src, "--host", host, "--port", strconv.Itoa(port), "--alias", alias)
	_ = s.Supervisor.Stop("llama-server") // exclusive: one model per worker
	// Launch via a login shell + exec, NOT a bare exec.Command: the direct
	// supervisor launch (new process group, null stdin) makes the Intel CPU
	// llama-server SEGFAULT, but it runs fine from a shell (same fix as the vLLM
	// supervisor hang). exec replaces sh so the supervisor still monitors the PID.
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = "'" + strings.ReplaceAll(a, "'", "'\\''") + "'"
	}
	_, err := s.Supervisor.Start(context.Background(), ProcessSpec{
		ID:          "llama-server",
		Command:     "/bin/sh",
		Args:        []string{"-lc", "exec llama-server " + strings.Join(quoted, " ")},
		Restart:     true,
		MaxRestarts: 5,
	})
	return err
}

// ---- process management endpoints ----
//
// Used by the leader's sharding orchestrator to launch rpc-server (and
// other helper processes) on workers without SSH. All endpoints are
// token-auth'd via the auth middleware.

// processStart spawns the process and returns 202 IMMEDIATELY, without waiting
// for its readiness probe. The caller polls /v1/process/get until the status
// leaves "starting".
//
// It used to call Supervisor.Start, which blocks until healthy — and that is
// unworkable over HTTP. A shard coordinator's ReadyTimeout is 15 minutes (a real
// cross-RPC model load) while the leader's client times out in 60s, so the leader
// reported a failed create for a process that was loading fine. Worse, Start was
// handed r.Context(): when the client gave up, the cancelled request context tore
// down the very process it was waiting for.
func (s *Server) processStart(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var spec ProcessSpec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	info, err := s.Supervisor.StartAsync(spec)
	if err != nil {
		// Registration errors only (bad spec, duplicate id). A spawn/readiness
		// failure arrives later on the process record as status "failed".
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": err.Error(),
			"info":  info,
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(info)
}

// processGet is the readiness poll that pairs with processStart's 202.
func (s *Server) processGet(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}
	info, ok := s.Supervisor.Get(id)
	if !ok {
		http.Error(w, "process "+id+" not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(info)
}

func (s *Server) processStop(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := s.Supervisor.Stop(req.ID); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) processList(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Supervisor.List())
}

func (s *Server) processLogs(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		http.Error(w, "id query param required", http.StatusBadRequest)
		return
	}
	n := 100
	if v := r.URL.Query().Get("lines"); v != "" {
		_, _ = fmt.Sscanf(v, "%d", &n)
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	for _, line := range s.Supervisor.Logs(id, n) {
		_, _ = io.WriteString(w, line+"\n")
	}
}

// fileCheck answers "do you already have this GGUF?". Used by the leader's
// sharding orchestrator to skip a multi-GB upload when the file is already
// present on the worker (e.g. the worker was a shard host last time too).
//
// Request:  HEAD /v1/process/file?name=<basename>&sha256=<hex>
// Response: 200 OK with header X-File-Path: <abs path on worker>
//
//	404 Not Found    — file missing or sha mismatch
//	503 if no ModelsDir is configured on this worker
//
// `name` must be a bare basename — no path separators. The worker resolves
// it under ModelsDir/<name> to prevent path-escape attacks even with a
// trusted leader.
func (s *Server) fileCheck(w http.ResponseWriter, r *http.Request) {
	if s.ModelsDir == "" {
		http.Error(w, "worker has no models_dir configured", http.StatusServiceUnavailable)
		return
	}
	name := r.URL.Query().Get("name")
	want := strings.ToLower(r.URL.Query().Get("sha256"))
	if name == "" || want == "" {
		http.Error(w, "name and sha256 required", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		http.Error(w, "name must be a basename", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.ModelsDir, name)
	got, err := sha256File(path)
	if err != nil || got != want {
		http.Error(w, "missing or sha mismatch", http.StatusNotFound)
		return
	}
	w.Header().Set("X-File-Path", path)
	w.WriteHeader(http.StatusOK)
}

// fileUpload streams a GGUF (or any opaque blob) from the request body into
// ModelsDir/<name>, verifying sha256 on completion. On mismatch the file is
// removed and 422 returned.
//
// Request:  POST /v1/process/upload?name=<basename>&sha256=<hex>
//
//	body: raw file contents (no multipart wrapper — Content-Length
//	      is the size)
//
// Response: 200 OK  {"path": "<abs path>", "sha256": "<hex>", "size": <n>}
//
//	422     {"error": "sha mismatch: got X want Y"}
//	503     if no ModelsDir is configured
//
// The file is written to a unique temp sibling first and renamed on success,
// so an interrupted upload doesn't leave a partial file the leader might
// see and skip on the next check, and two concurrent uploads of the same
// name can't interleave writes into a shared temp path.
func (s *Server) fileUpload(w http.ResponseWriter, r *http.Request) {
	if s.ModelsDir == "" {
		http.Error(w, "worker has no models_dir configured", http.StatusServiceUnavailable)
		return
	}
	defer r.Body.Close()
	name := r.URL.Query().Get("name")
	want := strings.ToLower(r.URL.Query().Get("sha256"))
	if name == "" || want == "" {
		http.Error(w, "name and sha256 required", http.StatusBadRequest)
		return
	}
	if strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
		http.Error(w, "name must be a basename", http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(s.ModelsDir, 0o755); err != nil {
		http.Error(w, "mkdir models_dir: "+err.Error(), http.StatusInternalServerError)
		return
	}

	finalPath := filepath.Join(s.ModelsDir, name)
	// Unique temp file per request: concurrent uploads of the same name must
	// not share one partial path, or they'd corrupt each other. Renamed over
	// finalPath only after the sha check passes.
	out, err := os.CreateTemp(s.ModelsDir, name+".partial-*")
	if err != nil {
		http.Error(w, "create: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tmpPath := out.Name()

	// No size cap on purpose: multi-GB GGUF uploads are the feature, and the
	// endpoint is token-gated.
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), r.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmpPath)
		http.Error(w, "write: "+copyErr.Error(), http.StatusBadGateway)
		return
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		http.Error(w, "close: "+closeErr.Error(), http.StatusInternalServerError)
		return
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		_ = os.Remove(tmpPath)
		http.Error(w, fmt.Sprintf("sha mismatch: got %s want %s", got, want), http.StatusUnprocessableEntity)
		return
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		_ = os.Remove(tmpPath)
		http.Error(w, "rename: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":   finalPath,
		"sha256": got,
		"size":   n,
	})
}

// sha256File reads path and returns the hex-encoded sha256, or an error if
// the file can't be read.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func writeAggregate(w http.ResponseWriter, stream <-chan engines.StreamEvent, model string) {
	defer func() {
		go func() {
			for range stream {
			}
		}()
	}()

	var text strings.Builder
	var usage *engines.Usage
	reason := "stop"
	for ev := range stream {
		if ev.Err != nil {
			http.Error(w, "engine: "+ev.Err.Error(), http.StatusBadGateway)
			return
		}
		if ev.Done {
			usage = ev.Usage
			if ev.Reason != "" {
				reason = ev.Reason
			}
			break
		}
		text.WriteString(ev.Delta)
	}
	resp := map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       map[string]string{"role": "assistant", "content": text.String()},
			"finish_reason": reason,
		}},
	}
	if usage != nil {
		resp["usage"] = map[string]int{
			"prompt_tokens":     usage.PromptTokens,
			"completion_tokens": usage.CompletionTokens,
			"total_tokens":      usage.TotalTokens,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
