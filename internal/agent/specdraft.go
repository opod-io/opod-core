package agent

// The draft model of speculative decoding (ADR-120, PLAN T29.19) is fetched
// like the served model: by this agent, with the same exclusive, atomic fetch,
// into the node's model cache, and the engine is handed the local path. The
// engine's own downloader is never used — it wrote into the shared cache
// beside other workers' and corrupted files (see launchLlamaServer).
//
// spec_draft names the draft as the plan wrote it:
//
//	vLLM, SGLang  <repo>[@<revision>]       a safetensors snapshot
//	llama.cpp     <repo>:<file>.gguf        one GGUF file
//	any engine    /an/absolute/path         already on the node: used as is

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/opod-io/opod-core/internal/fetch"
)

// resolveSpecDraft makes the plan's draft present on the node and rewrites
// spec_draft to its local path. A plan with no draft is left alone; a draft
// that cannot be fetched fails the launch, with the reason — the plan asked
// for it, and an engine started without it would serve without speculating.
func (s *Server) resolveSpecDraft(engine string) error {
	method, _, draft, ok := s.EngineFlags.spec()
	if !ok || method != SpecDraft || filepath.IsAbs(draft) {
		return nil
	}
	if s.ModelsDir == "" {
		return fmt.Errorf("speculative draft %s: no models directory to fetch it into", draft)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour) // a pull, not a request: a client that gives up must not abort it
	defer cancel()
	opt := fetch.Options{Log: slog.Default(), Token: s.HFToken, Endpoint: s.HFEndpoint}
	var path string
	var err error
	switch engine {
	case "llamacpp":
		repo, file, found := strings.Cut(draft, ":")
		if !found || repo == "" || file == "" {
			return fmt.Errorf("speculative draft %q: llama.cpp takes <repo>:<file>.gguf", draft)
		}
		path, err = fetch.GGUF(ctx, repo, file, s.ModelsDir, opt)
	case "vllm", "sglang":
		repo, rev, _ := strings.Cut(draft, "@")
		opt.Revision = rev
		path, err = fetch.Snapshot(ctx, repo, s.ModelsDir, opt)
	default:
		return fmt.Errorf("speculative draft %s: engine %s takes no draft model", draft, engine)
	}
	if err != nil {
		return fmt.Errorf("fetch speculative draft %s: %w", draft, err)
	}
	flags := make(EngineFlags, len(s.EngineFlags))
	for k, v := range s.EngineFlags {
		flags[k] = v
	}
	flags["spec_draft"] = path
	s.EngineFlags = flags
	return nil
}
