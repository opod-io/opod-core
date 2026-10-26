// Package trtllm is the TensorRT-LLM driver (ADR-092 §7, PLAN T19.15).
// `trtllm-serve <model>` serves an OpenAI-compatible API on the PyTorch backend
// — safetensors from the Hub, no engine build — so requests go through the
// shared openaicompat client, and this file is only what makes TensorRT-LLM
// different on that wire: its load signals live at /prometheus/metrics, and
// its reading of a json_schema response_format (responseFormat).
//
// The worker starts the server (agent/server.go, launchTRTLLM); the driver
// never does. One node only: TensorRT-LLM's multi-node mode needs MPI, which
// core does not carry, so a gang of this engine is refused by the control
// plane. No runtime LoRA, no tokenizer route, no sleep mode, and images are
// refused by name until a vision model is proven on it.
package trtllm

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/openaicompat"
)

const name = "trtllm"

func init() {
	engines.Register(engines.Descriptor{
		Name:        name,
		Aliases:     []string{"tensorrt-llm", "trtllm-serve"},
		New:         func(endpoint, apiKey string) engines.Engine { return New(endpoint, apiKey) },
		NativeName:  openaicompat.NativeName,
		SingleModel: true,
		CacheSalt:   true, // ChatCompletionRequest.cache_salt, passed to generate (release 1.2.1, read from the source)
		StartHint:   "start TensorRT-LLM (trtllm-serve <model> --port 8000) and ensure OPOD_TRTLLM_ENDPOINT matches",
	})
}

// Driver is an Engine that proxies to a running trtllm-serve.
type Driver struct {
	openaicompat.Client
	genRate engines.RateTracker
}

// New returns a driver for a trtllm-serve at endpoint (e.g. http://127.0.0.1:8000).
func New(endpoint, apiKey string) *Driver {
	var auth func(*http.Request)
	if apiKey != "" {
		auth = func(req *http.Request) { req.Header.Set("Authorization", "Bearer "+apiKey) }
	}
	c := openaicompat.NewClient(name, endpoint, auth)
	c.NoImages = "images are not proven on this engine yet (PLAN T16.2): serve vision models on vLLM, or send text only"
	c.CacheSalt = true
	return &Driver{Client: c}
}

func (d *Driver) Name() string { return name }

// Chat is the shared client's, with the response_format rewritten for this
// engine (responseFormat).
func (d *Driver) Chat(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	req.ResponseFormat = responseFormat(req.ResponseFormat)
	return d.Client.Chat(ctx, req)
}

// responseFormat turns OpenAI's {"type":"json_schema","json_schema":{"name",
// "schema",…}} into trtllm-serve's own {"type":"json","schema":<schema>}.
// Release 1.2.1 hands the whole json_schema object to its grammar as if it
// were the schema, so the {"name","schema"} wrapper constrains nothing and a
// schema-bound request comes back as any JSON at all (read on its
// openai_protocol.py; PLAN T27.8). Every other value passes as written, and so
// does a json_schema with no inner schema, which the engine then refuses itself.
func responseFormat(raw json.RawMessage) json.RawMessage {
	var rf struct {
		Type       string `json:"type"`
		JSONSchema struct {
			Schema json.RawMessage `json:"schema"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &rf); err != nil || rf.Type != "json_schema" || !engines.HasJSON(rf.JSONSchema.Schema) {
		return raw
	}
	out, err := json.Marshal(map[string]any{"type": "json", "schema": rf.JSONSchema.Schema})
	if err != nil {
		return raw
	}
	return out
}

// Pull is a no-op: the model is chosen when the server is launched.
func (d *Driver) Pull(context.Context, string, func(string, int64, int64)) error { return nil }

// Delete is a no-op (same reasoning as Pull).
func (d *Driver) Delete(context.Context, string) error { return nil }

// Unload is not supported: trtllm-serve owns one model per process.
func (d *Driver) Unload(context.Context, string) error { return engines.ErrUnloadNotSupported }

var _ engines.Engine = (*Driver)(nil)

// Load scrapes /prometheus/metrics: KV-cache utilisation (a 0–1 fraction),
// requests waiting, generated tokens/s between samples and the KV block reuse
// rate. Names moved between releases, so each signal reads the first alias.
func (d *Driver) Load(ctx context.Context) (engines.EngineLoad, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.BaseURL+"/prometheus/metrics", nil)
	if err != nil {
		return engines.EngineLoad{}, err
	}
	resp, err := d.HTTP.Do(req)
	if err != nil {
		return engines.EngineLoad{}, engines.Unreachable(name, d.BaseURL, err)
	}
	defer resp.Body.Close()
	now := time.Now()
	ld := engines.EngineLoad{SampledAt: now.Unix()}
	if resp.StatusCode == http.StatusNotFound {
		// Release 1.2.1 has no Prometheus route, and its /metrics JSON stays
		// empty unless iteration stats are switched on through a config file
		// (read on the cell, 2026-10-19): no load signal, which is not an
		// error. The control plane declares no KV pressure for it.
		return ld, nil
	}
	if resp.StatusCode != http.StatusOK {
		return engines.EngineLoad{}, engines.Upstream(name, "GET /prometheus/metrics", resp.StatusCode, nil)
	}
	m := engines.ParsePromText(resp.Body)
	if kv, ok := engines.First(m, "trtllm_kv_cache_utilization", "trtllm:kv_cache_utilization"); ok {
		ld.KVUsedPct = kv * 100
	}
	if q, ok := engines.First(m, "trtllm_num_requests_waiting", "trtllm:num_requests_waiting"); ok {
		ld.QueueDepth = int64(q)
	}
	if gen, ok := engines.First(m, "trtllm_generation_tokens_total", "trtllm:generation_tokens_total"); ok {
		ld.TokensPerSec = d.genRate.Rate(gen, now)
	}
	if hit, ok := engines.First(m, "trtllm_kv_cache_hit_rate", "trtllm:kv_cache_hit_rate"); ok {
		ld.PrefixHitPct = hit * 100
	}
	return ld, nil
}
