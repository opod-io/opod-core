package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/cache"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/models"
)

// ---- /v1/embeddings ----
//
// OpenAI-compatible embedding endpoint. Request shape:
//
//	{
//	  "model": "nomic-embed-text",
//	  "input": "the cat sat on the mat"          // OR ["sentence 1", "sentence 2"]
//	}
//
// Response shape:
//
//	{
//	  "object": "list",
//	  "data": [{"object": "embedding", "embedding": [floats…], "index": 0}, …],
//	  "model": "nomic-embed-text",
//	  "usage": {"prompt_tokens": 12, "total_tokens": 12}
//	}

type embeddingRequest struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"` // string or array of strings
	User  string          `json:"user,omitempty"`
}

type embeddingObject struct {
	Object    string    `json:"object"`
	Embedding []float32 `json:"embedding"`
	Index     int       `json:"index"`
}

type embeddingResponse struct {
	Object string            `json:"object"`
	Data   []embeddingObject `json:"data"`
	Model  string            `json:"model"`
	Usage  usage             `json:"usage"`
}

// Embeddings handles POST /v1/embeddings.
func (h *Handler) Embeddings(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	// Read the body once so we can also build a deterministic cache
	// key from it. Embedding requests are bounded in size (token-list
	// rather than a streaming attachment) so the double-buffering is
	// cheap.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}

	// Pre-call guardrails see the embedding input text too. Applied
	// before the cache lookup so a rewrite (e.g. PII masking) drives the
	// cache key and a blocked request can't be served from cache.
	body, ok := h.applyPreCallGuardrails(r.Context(), w, body)
	if !ok {
		// Guardrail blocked the request; response already written.
		return
	}

	// Cache lookup: embeddings are deterministic for fixed
	// (model, input), so this is the highest-ROI cache path. Skipped
	// when Cache-Control: no-cache / no-store is set, or when the
	// global cache isn't configured.
	if h.Policy().Cache != nil && !cacheBypass(r) {
		key := cache.KeyForRequest("/v1/embeddings", body, cacheNamespaceFromBody(body))
		if v, ok := h.Policy().Cache.Get(r.Context(), key); ok {
			metrics.ObserveCacheHit("embeddings")
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Opod-Cache", "hit")
			_, _ = w.Write(v)
			return
		}
		// Stash the key for later (post-handler) Set.
		r = r.WithContext(withEmbeddingCacheKey(r.Context(), key))
		metrics.ObserveCacheMiss("embeddings")
	}

	var req embeddingRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body: "+err.Error())
		return
	}
	if len(req.Input) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "input is required")
		return
	}

	inputs, err := parseEmbeddingInput(req.Input)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if len(inputs) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "input must contain at least one non-empty string")
		return
	}

	// The engine must implement EmbedEngine. The Router does this by
	// asserting on the picked backend; other engines that lack embedding
	// support surface as 501 here.
	ee, ok := h.Engine.(engines.EmbedEngine)
	if !ok {
		writeJSONError(w, http.StatusNotImplemented, "embeddings_not_supported",
			"the configured engine does not support embeddings")
		return
	}

	// Routing suffixes work here too (`nomic-embed-text:floor`), though
	// embedding chains are short — strip so the engine sees the base id.
	requested, sortHint := models.SplitSortSuffix(req.Model)
	if requested == "" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	resolved, err := h.ResolveModel(requested)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	ctx := overridesContext(r, nil, h.Store, requested, sortHint)
	start := time.Now()
	res, err := ee.Embed(ctx, engines.EmbedRequest{
		Model:  resolved,
		Inputs: inputs,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "upstream_error", err.Error())
		h.recordUsage(r.Context(), "openai", requested, nil, time.Since(start), "error")
		return
	}

	out := embeddingResponse{
		Object: "list",
		Model:  requested,
		Data:   make([]embeddingObject, 0, len(res.Vectors)),
	}
	for i, v := range res.Vectors {
		out.Data = append(out.Data, embeddingObject{
			Object:    "embedding",
			Embedding: v,
			Index:     i,
		})
	}
	if res.Usage != nil {
		out.Usage = usage{
			PromptTokens: res.Usage.PromptTokens,
			TotalTokens:  res.Usage.TotalTokens,
		}
	}

	// Record the call so quota + audit + cost analytics all see it.
	var u *engines.Usage
	if res.Usage != nil {
		u = &engines.Usage{PromptTokens: res.Usage.PromptTokens, TotalTokens: res.Usage.TotalTokens}
	}
	h.recordUsage(r.Context(), "openai", requested, u, time.Since(start), "ok")

	encoded, err := json.Marshal(out)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "encode_error", err.Error())
		return
	}
	if h.Policy().Cache != nil {
		if key, ok := embeddingCacheKeyFrom(r.Context()); ok {
			h.Policy().Cache.Set(r.Context(), key, encoded, 0) // use driver default TTL
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Opod-Cache", "miss")
	_, _ = w.Write(encoded)
}

// cacheBypass honors RFC-7234 Cache-Control directives for opt-out.
func cacheBypass(r *http.Request) bool {
	cc := r.Header.Get("Cache-Control")
	if cc == "" {
		return false
	}
	cc = strings.ToLower(cc)
	return strings.Contains(cc, "no-cache") || strings.Contains(cc, "no-store")
}

// cacheNamespaceFromBody pulls opod.cache.namespace from a JSON
// body. Falls back to "" — the cache key is still unique per body.
func cacheNamespaceFromBody(body []byte) string {
	var probe struct {
		Opod struct {
			Cache struct {
				Namespace string `json:"namespace"`
			} `json:"cache"`
		} `json:"opod"`
	}
	_ = json.Unmarshal(body, &probe)
	return probe.Opod.Cache.Namespace
}

type embeddingCacheKey struct{}

func withEmbeddingCacheKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, embeddingCacheKey{}, key)
}

func embeddingCacheKeyFrom(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(embeddingCacheKey{}).(string)
	return v, ok && v != ""
}

// parseEmbeddingInput accepts the OpenAI `input` field — either a single
// string or an array of strings — and returns a normalized list. Skips
// empty strings; returns an error for invalid JSON shape.
func parseEmbeddingInput(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	// Try single string.
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil, nil
		}
		return []string{single}, nil
	}
	// Try array of strings.
	var multi []string
	if err := json.Unmarshal(raw, &multi); err == nil {
		out := multi[:0]
		for _, s := range multi {
			if s != "" {
				out = append(out, s)
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("input must be a string or array of strings")
}
