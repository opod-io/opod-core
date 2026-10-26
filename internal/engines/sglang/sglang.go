// Package sglang is the SGLang driver. SGLang serves an OpenAI-compatible HTTP
// API (`python -m sglang.launch_server`, or the official image), so the request
// path is the shared openaicompat client and this file is only what makes
// SGLang itself different from every other server on that wire:
//
//   - its metrics have their own names, and its own prefix cache — RadixAttention
//     — is the reason an operator would pick it, so the hit rate has to be read
//     rather than left at zero;
//   - it has no sleep mode, so the sleep tier is refused by the interface rather
//     than by a comment (the control plane checks the capability before it plans
//     one);
//   - like vLLM it owns one model per process: the model is chosen at launch.
//
// The driver does not start or stop the server; the worker's supervisor does.
// Example launch (NVIDIA host):
//
//	docker run --gpus all -p 30000:30000 lmsysorg/sglang:latest \
//	  python3 -m sglang.launch_server --model-path Qwen/Qwen3-8B --host 0.0.0.0 --port 30000
package sglang

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

const name = "sglang"

func init() {
	engines.Register(engines.Descriptor{
		Name:        name,
		Aliases:     []string{"sgl"},
		New:         func(endpoint, apiKey string) engines.Engine { return New(endpoint, apiKey) },
		NativeName:  openaicompat.NativeName,
		SingleModel: true,
		StartHint:   "start SGLang (python -m sglang.launch_server --model-path <repo>) and ensure OPOD_SGLANG_ENDPOINT matches",
	})
}

// Driver is an Engine that proxies to a running SGLang server.
type Driver struct {
	openaicompat.Client
	genRate engines.RateTracker     // gen_throughput / generation_tokens_total → tokens/s between Load samples
	ttft    engines.HistogramWindow // time_to_first_token_seconds → the last minute's median (T24.25)
	// slots is the server's running-request limit, read once (Slots).
	slotsMu sync.Mutex
	slots   int
	// poolTokens is /server_info's max_total_num_tokens, read with the slots:
	// the KV pool when /metrics does not carry it.
	poolTokens int64
}

// New returns a driver for an SGLang server at endpoint (e.g. http://gpu:30000).
// apiKey is optional — set when the server was launched with --api-key.
func New(endpoint, apiKey string) *Driver {
	var auth func(*http.Request)
	if apiKey != "" {
		auth = func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+apiKey) }
	}
	c := openaicompat.NewClient(name, endpoint, auth)
	c.NoImages = "images are not proven on this engine yet (PLAN T16.2): serve vision models on vLLM, or send text only"
	return &Driver{Client: c}
}

func (s *Driver) Name() string { return name }

// Pull is a no-op: SGLang selects its model at server launch, exactly as vLLM
// does. Saying so is better than pretending to pull and reporting success.
func (s *Driver) Pull(ctx context.Context, modelID string, onProgress func(string, int64, int64)) error {
	if onProgress != nil {
		onProgress("sglang: model selection happens at server launch — no pull required", 0, 0)
	}
	return nil
}

// Delete is a no-op (same reasoning as Pull).
func (s *Driver) Delete(ctx context.Context, modelID string) error { return nil }

// Unload is not supported: SGLang owns one model per process. Restart it.
func (s *Driver) Unload(ctx context.Context, modelID string) error {
	return engines.ErrUnloadNotSupported
}

var _ engines.Engine = (*Driver)(nil)

// Load scrapes SGLang's /metrics so the autoscaler sees the same four signals it
// reads from every other engine (core build item 14). The names are SGLang's own,
// and each signal takes the first alias present because they have moved across
// releases. The prefix-cache hit rate matters more here than elsewhere: it is
// RadixAttention's whole point, and a fleet where it collapses is one that has
// lost the reason SGLang was chosen.
func (s *Driver) Load(ctx context.Context) (engines.EngineLoad, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+"/metrics", nil)
	if err != nil {
		return engines.EngineLoad{}, err
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return engines.EngineLoad{}, engines.Unreachable(name, s.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return engines.EngineLoad{}, engines.Upstream(name, "GET /metrics", resp.StatusCode, nil)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return engines.EngineLoad{}, engines.Unreachable(name, s.BaseURL, err)
	}
	m := engines.ParsePromText(bytes.NewReader(raw))
	now := time.Now()
	ld := engines.EngineLoad{SampledAt: now.Unix()}
	// The engine's own time to first token (PLAN T24.25).
	ld.EngineTTFTP50Ms = s.ttft.TTFTP50Ms(string(raw), "sglang:time_to_first_token_seconds", now)
	if kv, ok := engines.First(m, "sglang:token_usage", "sglang:kv_cache_usage_perc"); ok {
		ld.KVUsedPct = kv * 100
	}
	if q, ok := engines.First(m, "sglang:num_queue_reqs", "sglang:num_requests_waiting"); ok {
		ld.QueueDepth = int64(q)
	}
	// gen_throughput is already tokens/s; the counter is not, so it goes through
	// the rate tracker like vLLM's.
	if tps, ok := engines.First(m, "sglang:gen_throughput"); ok {
		ld.TokensPerSec = tps
	} else if gen, ok := engines.First(m, "sglang:generation_tokens_total"); ok {
		ld.TokensPerSec = s.genRate.Rate(gen, now)
	}
	// What the engine built and did (PLAN T23.2): the KV pool in tokens, the
	// tokens running requests hold in it (radix-cached tokens that may be
	// evicted are not counted), and its retractions — SGLang's preemption: a
	// running request sent back to the queue to free KV. SGLang counts no
	// evictions of its own (evictable tokens are a gauge), so those stay unsaid.
	if n, ok := engines.First(m, "sglang:max_total_num_tokens"); ok && n > 0 {
		ld.KVCapacityTokens = new(int64(n))
	} else if n := s.serverInfoPool(); n > 0 {
		ld.KVCapacityTokens = new(n)
	}
	if n, ok := engines.First(m, "sglang:kv_used_tokens", "sglang:num_used_tokens"); ok {
		ld.KVUsedTokens = new(int64(n))
	}
	if n, ok := engines.First(m, "sglang:num_retracted_requests_total"); ok {
		ld.PreemptionsTotal = new(int64(n))
	}
	if hit, ok := engines.First(m, "sglang:cache_hit_rate"); ok {
		// Released as a fraction in some versions and a percentage in others;
		// a rate above 1 can only be the latter.
		if hit <= 1 {
			hit *= 100
		}
		ld.PrefixHitPct = hit
	}
	return ld, nil
}

// Slots is how many requests SGLang runs at once: its scheduler's EFFECTIVE
// max running requests, which it derives from the KV pool when
// --max-running-requests is not given — so the number read back is the one the
// scheduler enforces, not the flag. Read once from /server_info (the older
// spelling /get_server_info answers the same body), summed over its
// data-parallel ranks: `internal_states[i].effective_max_running_requests_per_dp`,
// or `max_running_requests` on a release that predates that key.
func (s *Driver) Slots(ctx context.Context) (int, bool) {
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	if s.slots > 0 {
		return s.slots, true
	}
	for _, path := range []string{"/server_info", "/get_server_info"} {
		if n := s.readSlots(ctx, path); n > 0 {
			s.slots = n
			return n, true
		}
	}
	return 0, false
}

func (s *Driver) readSlots(ctx context.Context, path string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
	if err != nil {
		return 0
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var info struct {
		MaxTotalNumTokens int64 `json:"max_total_num_tokens"`
		InternalStates    []struct {
			Effective int `json:"effective_max_running_requests_per_dp"`
			Max       int `json:"max_running_requests"`
		} `json:"internal_states"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&info) != nil {
		return 0
	}
	if info.MaxTotalNumTokens > 0 {
		s.poolTokens = info.MaxTotalNumTokens
	}
	total := 0
	for _, st := range info.InternalStates {
		switch {
		case st.Effective > 0:
			total += st.Effective
		case st.Max > 0:
			total += st.Max
		}
	}
	return total
}

// serverInfoPool is the KV pool /server_info stated when the slots were read
// (Slots asks it once and keeps both); 0 = not read, or not said.
func (s *Driver) serverInfoPool() int64 {
	s.slotsMu.Lock()
	defer s.slotsMu.Unlock()
	return s.poolTokens
}

var _ engines.SlotReporter = (*Driver)(nil)

// Embeddings come from the shared OpenAI-compatible client this driver embeds.
var _ engines.EmbedEngine = (*Driver)(nil)

// Rerank serves a reranker's scores through SGLang's /v1/rerank (ADR-084),
// whose bare-array answer the shared decoder reads.
func (s *Driver) Rerank(ctx context.Context, req engines.RerankRequest) (engines.RerankResponse, error) {
	return openaicompat.Rerank(ctx, s.Client, req)
}

var _ engines.RerankEngine = (*Driver)(nil)

// Tokenize asks SGLang for the prompt's token ids (openaicompat.Tokenize):
// SGLang runs a /tokenize request with messages through its chat request's own
// processing, so the ids are the ones its radix cache is keyed by (T19.21).
func (s *Driver) Tokenize(ctx context.Context, model string, messages []engines.Message, prompt string, limit int) ([]int, error) {
	return openaicompat.Tokenize(ctx, s.Client, model, messages, prompt, limit)
}

var _ engines.Tokenizer = (*Driver)(nil)
