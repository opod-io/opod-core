// Package vllm is the vLLM driver. vLLM exposes an OpenAI-compatible HTTP
// server when launched with `python -m vllm.entrypoints.openai.api_server`
// or the official Docker image; the driver speaks OpenAI to it through
// openaicompat and adapts to Opod's Engine interface. Registers as "vllm"
// with aliases for Tenstorrent's tt-metal server ("tt-openai", "tenstorrent",
// "tt"), which exposes the same OpenAI API — no TT-specific code needed.
//
// The driver assumes the user (or the worker agent's supervisor) runs vLLM;
// it does not start/stop the process. Example launch (NVIDIA host):
//
//	docker run --gpus all -p 8000:8000 \
//	  vllm/vllm-openai:latest \
//	  --model Qwen/Qwen3-Coder-30B-A3B-Instruct-AWQ \
//	  --tensor-parallel-size 1
package vllm

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

const name = "vllm"

func init() {
	engines.Register(engines.Descriptor{
		Name:       name,
		Aliases:    []string{"tt-openai", "tenstorrent", "tt"},
		New:        func(endpoint, apiKey string) engines.Engine { return New(endpoint, apiKey) },
		NativeName: openaicompat.NativeName,
		StartHint:  "start vLLM (see https://docs.vllm.ai/) and ensure OPOD_VLLM_ENDPOINT matches",
	})
}

// Driver is an Engine that proxies to a running vLLM OpenAI-compatible server.
type Driver struct {
	openaicompat.Client
	genRate engines.RateTracker // generation_tokens_total → tokens/s between Load samples
}

// New returns a driver for a vLLM server at endpoint (e.g. http://gpu:8000).
// apiKey is optional — set when vLLM was launched with --api-key.
func New(endpoint, apiKey string) *Driver {
	var auth func(*http.Request)
	if apiKey != "" {
		auth = func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+apiKey) }
	}
	return &Driver{Client: openaicompat.NewClient(name, endpoint, auth)}
}

func (v *Driver) Name() string { return name }

// Pull is a no-op for vLLM: the model is fixed at server-launch time.
// The function returns nil immediately (the configured model is assumed loaded).
// A future version may shell out to `huggingface-cli download` to warm the
// HF cache before the user restarts vLLM with the new model.
func (v *Driver) Pull(ctx context.Context, modelID string, onProgress func(string, int64, int64)) error {
	if onProgress != nil {
		onProgress("vllm: model selection happens at server launch — no pull required", 0, 0)
	}
	return nil
}

// Delete is a no-op (same reasoning as Pull).
func (v *Driver) Delete(ctx context.Context, modelID string) error { return nil }

// Unload is not supported by vLLM — it owns one model per process.
// Restart the vLLM server to free its memory.
func (v *Driver) Unload(ctx context.Context, modelID string) error {
	return engines.ErrUnloadNotSupported
}

var _ engines.Engine = (*Driver)(nil)

// Load scrapes vLLM's own /metrics (build item 14): KV-cache usage,
// requests waiting for a slot, generated tokens/s since the last sample and
// the prefix-cache hit rate. Metric names moved across vLLM releases, so
// each signal reads the first alias present.
func (v *Driver) Load(ctx context.Context) (engines.EngineLoad, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.BaseURL+"/metrics", nil)
	if err != nil {
		return engines.EngineLoad{}, err
	}
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return engines.EngineLoad{}, engines.Unreachable(name, v.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return engines.EngineLoad{}, engines.Upstream(name, "GET /metrics", resp.StatusCode, nil)
	}
	m := engines.ParsePromText(resp.Body)
	now := time.Now()
	ld := engines.EngineLoad{SampledAt: now.Unix()}
	if kv, ok := engines.First(m, "vllm:kv_cache_usage_perc", "vllm:gpu_cache_usage_perc"); ok {
		ld.KVUsedPct = kv * 100
	}
	if q, ok := engines.First(m, "vllm:num_requests_waiting"); ok {
		ld.QueueDepth = int64(q)
	}
	if gen, ok := engines.First(m, "vllm:generation_tokens_total"); ok {
		ld.TokensPerSec = v.genRate.Rate(gen, now)
	}
	hits, hok := engines.First(m, "vllm:prefix_cache_hits_total", "vllm:gpu_prefix_cache_hits_total")
	queries, qok := engines.First(m, "vllm:prefix_cache_queries_total", "vllm:gpu_prefix_cache_queries_total")
	if hok && qok && queries > 0 {
		ld.PrefixHitPct = hits / queries * 100
	}
	return ld, nil
}

// Sleep / Resume / Sleeping drive vLLM's sleep mode (`--enable-sleep-mode`
// + VLLM_SERVER_DEV_MODE=1, which the worker sets when the control plane
// asks for the sleep tier): POST /sleep?level=1 keeps the weights in host
// memory so /wake_up is sub-second; /is_sleeping is the engine's truth.
func (v *Driver) Sleep(ctx context.Context) error { return v.postSleep(ctx, "/sleep?level=1") }

func (v *Driver) Resume(ctx context.Context) error { return v.postSleep(ctx, "/wake_up") }

func (v *Driver) postSleep(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.BaseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return engines.Unreachable(name, v.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return engines.Upstream(name, "POST "+path, resp.StatusCode, nil)
	}
	return nil
}

func (v *Driver) Sleeping(ctx context.Context) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.BaseURL+"/is_sleeping", nil)
	if err != nil {
		return false, err
	}
	resp, err := v.HTTP.Do(req)
	if err != nil {
		return false, engines.Unreachable(name, v.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, engines.Upstream(name, "GET /is_sleeping", resp.StatusCode, nil)
	}
	var out struct {
		IsSleeping bool `json:"is_sleeping"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false, err
	}
	return out.IsSleeping, nil
}
