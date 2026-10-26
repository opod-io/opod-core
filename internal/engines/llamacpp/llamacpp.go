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
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

const name = "llamacpp"

func init() {
	engines.Register(engines.Descriptor{
		Name:        name,
		Aliases:     []string{"llama-cpp", "llamacpp-rpc"},
		New:         func(endpoint, _ string) engines.Engine { return New(endpoint) },
		NativeName:  openaicompat.NativeName,
		SingleModel: true,
		StartHint:   "start llama.cpp: llama-server -m /path/to/model.gguf --port 8089",
	})
}

// Driver is an Engine pointing at a `llama-server` HTTP endpoint. The server
// may or may not be configured with --rpc — the driver doesn't care.
type Driver struct {
	openaicompat.Client
	genRate engines.RateTracker // tokens_predicted_total → tokens/s between Load samples
	// slots is /props total_slots and slotCtx each slot's context
	// (default_generation_settings.n_ctx), read once (totalSlots); slotsMu
	// guards both and the layout.
	slotsMu sync.Mutex
	slots   int
	slotCtx int64
	// unified is the KV layout the worker launched llama-server with, when it
	// said (SetKVUnified): nil = not told.
	unified *bool
}

// New returns a driver for a llama-server at endpoint.
func New(endpoint string) *Driver {
	c := openaicompat.NewClient(name, endpoint, nil)
	c.NoImages = "llama-server takes images only with a multimodal projector (--mmproj), which a worker does not start yet: serve vision models on vLLM, or send text only"
	return &Driver{Client: c}
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
// passes): KV-cache pressure, deferred requests (every slot busy) and predicted
// tokens/s since the last sample. llama-server reports no prefix-cache hit
// rate, so that stays 0.
//
// KV pressure is SLOT OCCUPANCY on this engine (PLAN T6.35, decided
// 2026-09-28): `requests_processing ÷ total_slots`. The image we ship emits
// no `kv_cache_usage_ratio` — read from inside a serving worker, its /metrics
// carries fifteen series and that is not one of them, so the field was 0 for
// every llama.cpp worker for ever and the autoscaler's KV trigger could not
// fire. With a partition per slot (`--no-kv-unified`: `-c` is the total, `-np`
// divides it) a busy slot IS a full partition, so occupancy is KV occupancy
// exactly. With one unified pool (`--kv-unified`, which a manager asks for on a
// worker that shares its card) it is NOT: a busy slot holds what its request
// holds, so this number is slot occupancy and the manager does not read it as
// KV use (G39 O-05). The ratio is still read first when a build emits it. The
// slot count comes from /props (`total_slots`), read once and kept.
// llama-server publishes no time-to-first-token histogram, so the engine's own
// TTFT (EngineTTFTP50Ms, PLAN T24.25) is never stated here — not guessed from
// its per-request timings either.
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
	} else if busy, ok := engines.First(m, "llamacpp:requests_processing"); ok {
		if slots := l.totalSlots(ctx); slots > 0 {
			ld.KVUsedPct = min(100, busy/float64(slots)*100)
		}
	}
	ld.KVCapacityTokens = l.kvPool(ctx)
	if q, ok := engines.First(m, "llamacpp:requests_deferred"); ok {
		ld.QueueDepth = int64(q)
	}
	if gen, ok := engines.First(m, "llamacpp:tokens_predicted_total"); ok {
		ld.TokensPerSec = l.genRate.Rate(gen, now)
	}
	return ld, nil
}

// totalSlots is the server's slot count from /props, read once: it is fixed
// for the process's life (`-np`). 0 when the server does not say.
func (l *Driver) totalSlots(ctx context.Context) int {
	l.slotsMu.Lock()
	defer l.slotsMu.Unlock()
	if l.slots > 0 {
		return l.slots
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.BaseURL+"/props", nil)
	if err != nil {
		return 0
	}
	resp, err := l.HTTP.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var props struct {
		TotalSlots int `json:"total_slots"`
		Defaults   struct {
			NCtx int64 `json:"n_ctx"`
		} `json:"default_generation_settings"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&props) != nil {
		return 0
	}
	l.slots, l.slotCtx = props.TotalSlots, props.Defaults.NCtx
	return l.slots
}

// SetKVUnified tells the driver how the llama-server it reads was launched:
// one KV pool every slot shares (--kv-unified), or a partition per slot. The
// worker that launched it knows; /props does not say, and the pool cannot be
// read without it (kvPool).
func (l *Driver) SetKVUnified(unified bool) {
	l.slotsMu.Lock()
	defer l.slotsMu.Unlock()
	l.unified = &unified
}

// kvPool is the KV pool llama-server built, in tokens (PLAN T23.2), from its
// own /props: `default_generation_settings.n_ctx` is ONE slot's context, so
// the pool is that times total_slots when each slot has its own partition, and
// that alone when the slots share a unified pool (one sequence may take all
// of it). With one slot the two are the same. nil when the server does not
// say, or when it has several slots and nobody said which layout they have —
// a pool off by the slot count is worse than none. (A slot capped by
// --kv-unified-per-slot reports its cap; the worker then tells no layout.) llama.cpp has no
// preemption, counts no tokens in use and no evictions, so those stay unsaid.
func (l *Driver) kvPool(ctx context.Context) *int64 {
	slots := l.totalSlots(ctx)
	l.slotsMu.Lock()
	defer l.slotsMu.Unlock()
	switch {
	case slots <= 0 || l.slotCtx <= 0:
		return nil
	case slots == 1 || (l.unified != nil && *l.unified):
		return new(l.slotCtx)
	case l.unified != nil:
		return new(l.slotCtx * int64(slots))
	}
	return nil
}

var _ engines.KVLayoutSetter = (*Driver)(nil)

// Slots is the server's slot count (`total_slots` in /props, set by `-np`):
// how many requests llama-server decodes at once. Anything past it waits in
// llama-server's own queue, where no class can rank it (ADR-091).
func (l *Driver) Slots(ctx context.Context) (int, bool) {
	n := l.totalSlots(ctx)
	return n, n > 0
}

var _ engines.SlotReporter = (*Driver)(nil)

// Embeddings come from the shared OpenAI-compatible client this driver embeds.
var _ engines.EmbedEngine = (*Driver)(nil)

// Rerank serves a reranker GGUF's scores (ADR-084). llama-server must be
// started with --reranking; without it the server answers 501, which comes
// back as engines.ErrRerankNotSupported with the server's own words.
func (l *Driver) Rerank(ctx context.Context, req engines.RerankRequest) (engines.RerankResponse, error) {
	return openaicompat.Rerank(ctx, l.Client, req)
}

var _ engines.RerankEngine = (*Driver)(nil)
