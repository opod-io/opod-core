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
	"context"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

const name = "sglang"

func init() {
	engines.Register(engines.Descriptor{
		Name:       name,
		Aliases:    []string{"sgl"},
		New:        func(endpoint, apiKey string) engines.Engine { return New(endpoint, apiKey) },
		NativeName: openaicompat.NativeName,
		StartHint:  "start SGLang (python -m sglang.launch_server --model-path <repo>) and ensure OPOD_SGLANG_ENDPOINT matches",
	})
}

// Driver is an Engine that proxies to a running SGLang server.
type Driver struct {
	openaicompat.Client
	genRate engines.RateTracker // gen_throughput / generation_tokens_total → tokens/s between Load samples
}

// New returns a driver for an SGLang server at endpoint (e.g. http://gpu:30000).
// apiKey is optional — set when the server was launched with --api-key.
func New(endpoint, apiKey string) *Driver {
	var auth func(*http.Request)
	if apiKey != "" {
		auth = func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+apiKey) }
	}
	return &Driver{Client: openaicompat.NewClient(name, endpoint, auth)}
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
	m := engines.ParsePromText(resp.Body)
	now := time.Now()
	ld := engines.EngineLoad{SampledAt: now.Unix()}
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

// Embeddings come from the shared OpenAI-compatible client this driver embeds.
var _ engines.EmbedEngine = (*Driver)(nil)
