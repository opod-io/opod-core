// Package api implements the public HTTP surface. OpenAI-compatible
// (/v1/models, /v1/chat/completions) and Anthropic-compatible
// (/v1/messages, /v1/messages/count_tokens) live in this package.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// Handler holds dependencies for the OpenAI-compatible routes.
type Handler struct {
	Engine  engines.Engine
	Store   store.Store
	Catalog []models.Entry
	Default string // default model when request.model is "" or "auto"
}

// ---- /v1/models ----

type modelObj struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

type modelList struct {
	Object string     `json:"object"`
	Data   []modelObj `json:"data"`
}

// ListModels handles GET /v1/models.
func (h *Handler) ListModels(w http.ResponseWriter, r *http.Request) {
	installed, err := h.Store.Models().List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "store_error", err.Error())
		return
	}
	out := modelList{Object: "list"}
	seen := map[string]bool{}
	for _, m := range installed {
		seen[m.CatalogID] = true
		out.Data = append(out.Data, modelObj{
			ID:      m.CatalogID,
			Object:  "model",
			Created: m.InstalledAt.Unix(),
			OwnedBy: "opod",
		})
	}
	// Models resident only on workers (heartbeat placements) are routable, so
	// list them too — otherwise a router-only leader advertises nothing.
	if nodes, err := h.Store.Nodes().List(r.Context()); err == nil {
		for _, n := range nodes {
			ps, err := h.Store.Placements().GetByNode(r.Context(), n.ID)
			if err != nil {
				continue
			}
			for _, p := range ps {
				if seen[p.ModelID] || (p.Status != "" && p.Status != "ready") {
					continue
				}
				seen[p.ModelID] = true
				out.Data = append(out.Data, modelObj{ID: p.ModelID, Object: "model", Created: p.LastSeen.Unix(), OwnedBy: "opod"})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- /v1/chat/completions ----

type chatRequest struct {
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Stream      bool          `json:"stream,omitempty"`
	Temperature *float32      `json:"temperature,omitempty"`
	TopP        *float32      `json:"top_p,omitempty"`
	MaxTokens   *int          `json:"max_tokens,omitempty"`
	Stop        []string      `json:"stop,omitempty"`
	User        string        `json:"user,omitempty"`
	// Opod is the namespaced bag for per-request routing overrides —
	// fallbacks, retry count, retry backoff. Nested rather than top-level
	// so we don't risk shadowing future OpenAI fields. Equivalent
	// `X-Opod-*` headers work too; body wins on conflict.
	Opod *opodExtras `json:"opod,omitempty"`
}

// opodExtras is the optional `opod.*` block carried inside an
// otherwise OpenAI/Anthropic-shaped request. Each field is mirrored by
// a header for clients that can only inject headers (curl one-liners,
// proxy-rewrites).
type opodExtras struct {
	Fallbacks      []string `json:"fallbacks,omitempty"`
	NumRetries     int      `json:"num_retries,omitempty"`
	RetryBackoffMS int      `json:"retry_backoff_ms,omitempty"`
	Hedge          bool     `json:"hedge,omitempty"`
	// Sort reorders the candidate chain by a metric: price | latency |
	// throughput. The `:floor` / `:nitro` model-name suffixes are
	// shortcuts for price / throughput; this explicit field wins over
	// a suffix when both are present.
	Sort  string `json:"sort,omitempty"`
	Cache *struct {
		Namespace string `json:"namespace,omitempty"`
	} `json:"cache,omitempty"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is either a JSON string (text-only) or a JSON array of content
	// parts per the OpenAI multimodal spec, e.g.
	//   [{"type":"text","text":"…"},
	//    {"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}]
	// Parsed by toEngineMessages.
	Content json.RawMessage `json:"content"`
}

// chatContentPart is one element of the OpenAI multimodal content array.
type chatContentPart struct {
	Type     string               `json:"type"`
	Text     string               `json:"text,omitempty"`
	ImageURL *chatContentImageURL `json:"image_url,omitempty"`
}

type chatContentImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"` // low | high | auto — ignored for now
}

type chatResponse struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	Created           int64        `json:"created"`
	Model             string       `json:"model"`
	SystemFingerprint string       `json:"system_fingerprint,omitempty"`
	Choices           []chatChoice `json:"choices"`
	Usage             usage        `json:"usage"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatChunk struct {
	ID      string            `json:"id"`
	Object  string            `json:"object"`
	Created int64             `json:"created"`
	Model   string            `json:"model"`
	Choices []chatChunkChoice `json:"choices"`
	Usage   *usage            `json:"usage,omitempty"`
}

type chatChunkChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason *string        `json:"finish_reason"`
}

// ChatCompletions handles POST /v1/chat/completions, both streaming and not.
func (h *Handler) ChatCompletions(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	// Read the body so pre-call guardrails can inspect (or rewrite)
	// it before we decode. The hot-path short-circuits when no
	// guardrails are configured.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
		return
	}
	rewritten, ok := applyPreCallGuardrails(r.Context(), w, h.Store, body)
	if !ok {
		// Guardrail blocked the request; response already written.
		return
	}
	var req chatRequest
	if err := json.Unmarshal(rewritten, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "Invalid JSON body: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeJSONError(w, http.StatusBadRequest, "invalid_request", "messages cannot be empty")
		return
	}

	// Strip an OpenRouter-style `:floor` / `:nitro` routing suffix before
	// model resolution — it's a routing hint, not part of the model id.
	// Usage records and the response `model` field carry the base id.
	requested, sortHint := models.SplitSortSuffix(req.Model)
	if requested == "" || requested == "auto" {
		requested = h.Default
	}
	// Re-check the per-key allowlist on the post-substitution model.
	// ModelAllowMiddleware passes empty/`auto` model requests through,
	// so the default must be authorized for this key too.
	if !modelAllowedForKey(r.Context(), requested) {
		key := auth.KeyFrom(r.Context())
		auditRefusal(r.Context(), h.Store, key, requested)
		writeModelNotAllowed(w, requested, key.AllowedModels)
		return
	}
	// Validate the model exists in the catalog, but do NOT substitute the
	// engine-native name here. The router routes by CATALOG id (placements are
	// catalog-keyed) and resolves to the native name at dispatch — locally via
	// SetLocalResolver, or on the worker for remote placements. Passing the
	// native name here made the router query placements by native name, never
	// match a worker, and fall through to the local engine → 404.
	if _, err := h.ResolveModel(requested); err != nil {
		writeJSONError(w, http.StatusNotFound, "model_not_found", err.Error())
		return
	}

	engineReq := engines.ChatRequest{
		Model:       requested,
		Messages:    toEngineMessages(req.Messages),
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   req.MaxTokens,
		Stop:        req.Stop,
		Stream:      true, // we always stream from the engine; aggregate if needed
	}

	// Per-request routing overrides (opod.fallbacks / opod.num_retries /
	// opod.retry_backoff_ms / opod.sort in the body or X-Opod-*
	// headers, plus any :floor/:nitro suffix) attach to the context the
	// router reads.
	ctx := overridesContext(r, req.Opod, h.Store, requested, sortHint)

	start := time.Now()
	stream, err := h.Engine.Chat(ctx, engineReq)
	if err != nil {
		recordUsage(r.Context(), h.Store, "openai", requested, nil, time.Since(start), "error")
		if status, code, msg, ok := upstreamPassthrough(err); ok {
			// The engine answered with a status the caller must see as-is
			// (rate limit, not ready, bad request, unknown model): relay it —
			// a remote endpoint's leader is a proxy, not the origin of the fault.
			if status == http.StatusServiceUnavailable || status == http.StatusTooManyRequests {
				w.Header().Set("Retry-After", "10")
			}
			writeJSONError(w, status, code, msg)
			return
		}
		code, msg := classifyEngineError(h.Engine, err)
		writeJSONError(w, http.StatusBadGateway, code, msg)
		return
	}

	id := "chatcmpl-" + randID()
	created := time.Now().Unix()

	if req.Stream {
		h.streamResponse(w, r, stream, id, created, requested, start)
		return
	}
	h.aggregateResponse(w, r, stream, id, created, requested, start)
}

func (h *Handler) streamResponse(w http.ResponseWriter, r *http.Request,
	stream <-chan engines.StreamEvent, id string, created int64, modelOut string, start time.Time) {

	// Drain the stream channel on exit so the engine producer never blocks on
	// a full buffer when the client disconnects. The engine goroutine respects
	// ctx and will exit promptly; this is belt-and-suspenders.
	defer drainStream(stream)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, _ := w.(http.Flusher)

	// first chunk announces the assistant role
	sendChunk(w, flusher, chatChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: modelOut,
		Choices: []chatChunkChoice{{
			Index: 0,
			Delta: map[string]any{"role": "assistant"},
		}},
	})

	for ev := range stream {
		// Bail before writing if the client is gone — avoids broken-pipe noise.
		if r.Context().Err() != nil {
			recordUsage(r.Context(), h.Store, "openai", modelOut, nil, time.Since(start), "cancelled")
			return
		}
		if ev.Err != nil {
			recordUsage(r.Context(), h.Store, "openai", modelOut, nil, time.Since(start), "error")
			writeSSEError(w, flusher, h.Engine, ev.Err)
			return
		}
		if ev.Done {
			reason := ev.Reason
			if reason == "" {
				reason = "stop"
			}
			finalChunk := chatChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: modelOut,
				Choices: []chatChunkChoice{{
					Index:        0,
					Delta:        map[string]any{},
					FinishReason: &reason,
				}},
			}
			if ev.Usage != nil {
				finalChunk.Usage = &usage{
					PromptTokens:     ev.Usage.PromptTokens,
					CompletionTokens: ev.Usage.CompletionTokens,
					TotalTokens:      ev.Usage.TotalTokens,
				}
			}
			sendChunk(w, flusher, finalChunk)
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			recordUsage(r.Context(), h.Store, "openai", modelOut, ev.Usage, time.Since(start), "ok")
			return
		}
		if ev.Delta != "" {
			sendChunk(w, flusher, chatChunk{
				ID: id, Object: "chat.completion.chunk", Created: created, Model: modelOut,
				Choices: []chatChunkChoice{{
					Index: 0,
					Delta: map[string]any{"content": ev.Delta},
				}},
			})
		}
	}
}

// drainStream consumes any remaining events on the channel in a goroutine so
// the producer never blocks. The producer also respects ctx, so this returns
// quickly in practice.
func drainStream(stream <-chan engines.StreamEvent) {
	go func() {
		for range stream {
		}
	}()
}

func (h *Handler) aggregateResponse(w http.ResponseWriter, r *http.Request, stream <-chan engines.StreamEvent,
	id string, created int64, modelOut string, start time.Time) {
	defer drainStream(stream)

	var text string
	var u *engines.Usage
	reason := "stop"
	for ev := range stream {
		if ev.Err != nil {
			recordUsage(r.Context(), h.Store, "openai", modelOut, nil, time.Since(start), "error")
			writeJSONError(w, http.StatusBadGateway, "upstream_error", ev.Err.Error())
			return
		}
		if ev.Done {
			u = ev.Usage
			if ev.Reason != "" {
				reason = ev.Reason
			}
			break
		}
		text += ev.Delta
	}
	resp := chatResponse{
		ID: id, Object: "chat.completion", Created: created, Model: modelOut,
		Choices: []chatChoice{{
			Index:        0,
			Message:      chatMessage{Role: "assistant", Content: jsonString(text)},
			FinishReason: reason,
		}},
	}
	if u != nil {
		resp.Usage = usage{
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			TotalTokens:      u.TotalTokens,
		}
	}
	writeJSON(w, http.StatusOK, resp)
	recordUsage(r.Context(), h.Store, "openai", modelOut, u, time.Since(start), "ok")
}

// ResolveModel maps the OpenAI "model" field to the engine-native identifier.
// If the requested ID matches a catalog entry, the engine-specific name
// (e.g. ollama_name) is returned; otherwise the input is passed through so users
// can specify raw engine model names directly.
func (h *Handler) ResolveModel(catalogID string) (string, error) {
	if catalogID == "" {
		return "", fmt.Errorf("no model specified and no default configured")
	}
	if eng, ok := h.lookupModelByCatalogID(catalogID); ok {
		return eng, nil
	}
	return catalogID, nil
}

// lookupModelByCatalogID picks the engine-native model name for the configured
// engine. Catalog entries can carry both an Ollama tag (source.ollama_name) and
// an HF repo (source.repo); we pick the right one based on which engine the
// gateway is talking to.
func (h *Handler) lookupModelByCatalogID(catalogID string) (engineModel string, ok bool) {
	for _, e := range h.Catalog {
		if e.ID != catalogID {
			continue
		}
		switch h.Engine.Name() {
		case "ollama":
			if e.Source.OllamaName != "" {
				return e.Source.OllamaName, true
			}
		case "vllm", "mlx", "mlx-lm":
			if e.Source.Repo != "" {
				return e.Source.Repo, true
			}
			if e.Source.Path != "" {
				return e.Source.Path, true
			}
		}
		return e.ID, true
	}
	return "", false
}

// ---- helpers ----

func toEngineMessages(in []chatMessage) []engines.Message {
	out := make([]engines.Message, 0, len(in))
	for _, m := range in {
		text, images := parseChatContent(m.Content)
		out = append(out, engines.Message{Role: m.Role, Content: text, Images: images})
	}
	return out
}

// parseChatContent accepts OpenAI's `content` field in either form:
//
//   - a plain JSON string                        → text, no images
//   - a JSON array of {type, text|image_url} parts → text parts concatenated
//     with a single space, image_url URLs collected into images
//
// Anything that fails to parse is treated as empty text — the engine layer
// will produce a clear error if the resulting prompt is empty.
func parseChatContent(raw json.RawMessage) (text string, images []string) {
	if len(raw) == 0 {
		return "", nil
	}
	// Try string first.
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return asStr, nil
	}
	// Try array of content parts.
	var parts []chatContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", nil
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(p.Text)
		case "image_url":
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				images = append(images, stripDataURLPrefix(p.ImageURL.URL))
			}
		}
	}
	return b.String(), images
}

// jsonString returns a json.RawMessage holding the JSON encoding of s — so
// a chatMessage built for a response renders `"content": "…"` instead of the
// raw byte slice. Errors here are unreachable (string marshal always works).
func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

// stripDataURLPrefix turns "data:image/png;base64,iVBORw0…" into the raw base64
// payload. Engines like Ollama want just the base64 bytes; vLLM accepts either.
// Non-data URLs (https://…) are returned unchanged.
func stripDataURLPrefix(s string) string {
	if !strings.HasPrefix(s, "data:") {
		return s
	}
	if i := strings.Index(s, ","); i >= 0 {
		return s[i+1:]
	}
	return s
}

func sendChunk(w http.ResponseWriter, flusher http.Flusher, c chatChunk) {
	b, _ := json.Marshal(c)
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(b)
	_, _ = io.WriteString(w, "\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func writeSSEError(w http.ResponseWriter, flusher http.Flusher, eng engines.Engine, err error) {
	code, msg := classifyEngineError(eng, err)
	body := map[string]any{"error": map[string]any{"type": code, "message": msg}}
	b, _ := json.Marshal(body)
	_, _ = io.WriteString(w, "data: ")
	_, _ = w.Write(b)
	_, _ = io.WriteString(w, "\n\n")
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

// classifyEngineError inspects the engine error and returns a typed
// code + an actionable message. Connection-refused / timeout / no-such-
// host all collapse to "engine_unreachable" with the engine name + URL
// + start-hint so the API consumer (and their UI) can show something
// better than "engine error: dial tcp …".
func classifyEngineError(eng engines.Engine, err error) (code, msg string) {
	s := strings.ToLower(err.Error())
	unreachable := errors.Is(err, engines.ErrUnreachable) ||
		strings.Contains(s, "connection refused") ||
		strings.Contains(s, "no such host") ||
		strings.Contains(s, "i/o timeout") ||
		strings.Contains(s, "context deadline exceeded") ||
		strings.Contains(s, "connect: ") ||
		strings.Contains(s, "eof")
	if !unreachable {
		return "upstream_error", err.Error()
	}
	hint := engineRestartHint(eng.Name())
	return "engine_unreachable", fmt.Sprintf("%s at %s is not reachable (%v). %s",
		eng.Name(), eng.Endpoint(), err, hint)
}

// upstreamPassthrough decides whether an engine's non-2xx answer is relayed
// with its own status. Client-side statuses (400, 404, 413, 422, 429) and
// "not ready" (503) are the caller's business; auth failures against the
// engine (401/403) and server faults stay 502 — those are ours to fix.
// The message is the upstream's own `error.message` when the body is
// OpenAI-shaped, else the trimmed body.
func upstreamPassthrough(err error) (status int, code, msg string, ok bool) {
	var up *engines.UpstreamError
	if !errors.As(err, &up) {
		return 0, "", "", false
	}
	switch up.Status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity,
		http.StatusTooManyRequests, http.StatusServiceUnavailable:
	default:
		return 0, "", "", false
	}
	msg = up.Body
	var shaped struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(up.Body), &shaped) == nil && shaped.Error.Message != "" {
		msg = shaped.Error.Message
	}
	if msg == "" {
		msg = http.StatusText(up.Status)
	}
	code = "upstream_" + strconv.Itoa(up.Status)
	if shaped.Error.Type != "" {
		code = shaped.Error.Type
	}
	return up.Status, code, up.Engine + ": " + msg, true
}

// engineRestartHint renders the driver's StartHint (owned by the driver
// package, shared with cmd/opod) as a sentence.
func engineRestartHint(name string) string {
	if _, known := engines.Lookup(name); !known {
		return "Check the engine endpoint in `~/.opod/config.yaml` and confirm it is listening."
	}
	hint := engines.StartHint(name)
	return strings.ToUpper(hint[:1]) + hint[1:] + "."
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, code, msg string) {
	body := map[string]any{"error": map[string]any{"type": code, "message": msg}}
	writeJSON(w, status, body)
}

func randID() string {
	buf := make([]byte, 9)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
