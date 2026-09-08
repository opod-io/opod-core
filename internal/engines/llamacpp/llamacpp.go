// Package llamacpp is the llama.cpp driver. Two modes, one driver:
//
//   - Single-node: `llama-server -m model.gguf --port 8089` on the same box
//     as Opod. Set `engine.preferred: llamacpp` (or `OPOD_ENGINE=llamacpp`)
//     and point `engine.llamacpp_endpoint` at it. Lower RAM and cold-start
//     latency than Ollama on weak hardware — bare llama.cpp, no daemon
//     layer in between.
//
//   - Distributed (RPC): `llama-server --rpc <backends>` shards a single
//     model across multiple machines via `rpc-server` processes. The
//     sharding orchestrator (`internal/scheduler/sharding.go`) launches
//     `rpc-server` on workers automatically and starts the coordinator
//     `llama-server`, then points an internal llamacpp driver at the
//     coordinator. You can also run it by hand and point Opod at the
//     result via `engine.preferred: llamacpp`.
//
// The driver is a thin OpenAI-compatible client (openaicompat, same shape
// as vLLM/MLX); from its perspective the upstream is just an OpenAI server,
// --rpc or not. Registers as "llamacpp" with aliases "llama-cpp" and
// "llamacpp-rpc".
//
// Manual RPC setup (if you prefer to manage processes yourself):
//
//	# On each worker node:
//	rpc-server -p 50052 &
//
//	# On the coordinator (the machine that exposes the OpenAI API):
//	llama-server -m /path/to/model.gguf \
//	    --rpc worker1.local:50052,worker2.local:50052 \
//	    --gpu-layers 999 --port 8089
//
//	# On the Opod leader, configure the catalog entry with
//	# source.type: llamacpp_rpc
//	# source.path: /path/to/model.gguf
//	# and set engine.preferred: llamacpp on the coordinator.
//
// Reference: https://github.com/ggerganov/llama.cpp/tree/master/examples/rpc
package llamacpp

import (
	"context"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

const name = "llamacpp"

func init() {
	engines.Register(engines.Descriptor{
		Name:       name,
		Aliases:    []string{"llama-cpp", "llamacpp-rpc"},
		New:        func(endpoint, _ string) engines.Engine { return New(endpoint) },
		NativeName: openaicompat.NativeName,
		StartHint:  "start llama.cpp: llama-server -m /path/to/model.gguf --port 8089",
	})
}

// Driver is an Engine pointing at a `llama-server` HTTP endpoint. The server
// may or may not be configured with --rpc — the driver doesn't care.
type Driver struct {
	openaicompat.Client
	genRate engines.RateTracker // tokens_predicted_total → tokens/s between Load samples
}

// New returns a driver for a llama-server at endpoint.
func New(endpoint string) *Driver {
	return &Driver{Client: openaicompat.NewClient(name, endpoint, nil)}
}

func (l *Driver) Name() string { return name }

// Health uses llama-server's dedicated /health route (200 once the model is
// loaded; 503 while loading), which is more truthful than /v1/models.
func (l *Driver) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return engines.Unreachable(name, l.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return engines.Upstream(name, "GET /health", resp.StatusCode, nil)
	}
	return nil
}

// Pull is a no-op (model is selected at llama-server launch time).
// A future scheduler may shell out to download GGUFs to coordinator + worker
// nodes, but that's coordination, not driver responsibility.
func (l *Driver) Pull(ctx context.Context, modelID string, onProgress func(string, int64, int64)) error {
	if onProgress != nil {
		onProgress("llamacpp: model selection happens at llama-server launch", 0, 0)
	}
	return nil
}

// Delete is a no-op for the same reason.
func (l *Driver) Delete(ctx context.Context, modelID string) error { return nil }

// Unload is not supported by llama-server — it owns one model per process.
// When Opod auto-spawned llama-server, `opod up`'s supervisor stops the
// process on shutdown (which frees memory). User-managed llama-server must
// be restarted to free RAM.
func (l *Driver) Unload(ctx context.Context, modelID string) error {
	return engines.ErrUnloadNotSupported
}

var _ engines.Engine = (*Driver)(nil)

// Load scrapes llama-server's /metrics (needs `--metrics`, which the worker
// passes): KV-cache usage, deferred requests (every slot busy) and predicted
// tokens/s since the last sample. llama-server reports no prefix-cache hit
// rate, so that stays 0.
func (l *Driver) Load(ctx context.Context) (engines.EngineLoad, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/metrics", nil)
	if err != nil {
		return engines.EngineLoad{}, err
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return engines.EngineLoad{}, engines.Unreachable(name, l.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return engines.EngineLoad{}, engines.Upstream(name, "GET /metrics", resp.StatusCode, nil)
	}
	m := engines.ParsePromText(resp.Body)
	now := time.Now()
	ld := engines.EngineLoad{SampledAt: now.Unix()}
	if kv, ok := engines.First(m, "llamacpp:kv_cache_usage_ratio"); ok {
		ld.KVUsedPct = kv * 100
	}
	if q, ok := engines.First(m, "llamacpp:requests_deferred"); ok {
		ld.QueueDepth = int64(q)
	}
	if gen, ok := engines.First(m, "llamacpp:tokens_predicted_total"); ok {
		ld.TokensPerSec = l.genRate.Rate(gen, now)
	}
	return ld, nil
}
