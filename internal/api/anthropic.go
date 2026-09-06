// Anthropic Messages API adapter. Implements POST /v1/messages and
// POST /v1/messages/count_tokens, mapping to and from the engine-native
// chat format. This is what makes Claude Code (and the Anthropic SDK)
// work against Opod.
//
// Reference: https://docs.anthropic.com/en/api/messages
//
// Currently supports:
//   - text content (string or array of text blocks)
//   - system prompt (string)
//   - streaming via SSE events (message_start, content_block_*, message_delta, message_stop)
//   - tools (tool_use / tool_result content blocks, with Input + Content preserved)
//
// Not implemented (open issues):
//   - extended thinking (`thinking` content blocks)
//   - prompt caching (cache_control)
//   - computer use
//   - vision (image content blocks)
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
)

// AnthropicHandler holds dependencies for the Anthropic-compatible routes.
// It reuses Handler.Engine and Handler.Catalog via composition.
type AnthropicHandler struct {
	*Handler
}

// ---- request types ----

type anthropicRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	System      json.RawMessage    `json:"system,omitempty"` // string or array of text blocks
	Messages    []anthropicMessage `json:"messages"`
	Stream      bool               `json:"stream,omitempty"`
	Temperature *float32           `json:"temperature,omitempty"`
	TopP        *float32           `json:"top_p,omitempty"`
	StopSeq     []string           `json:"stop_sequences,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	ToolChoice  json.RawMessage    `json:"tool_choice,omitempty"`
	// Opod is the namespaced bag for per-request routing overrides
	// (fallbacks, retry count, retry backoff). Same shape and semantics
	// as on the OpenAI handler; `X-Opod-*` headers work too.
	Opod *opodExtras `json:"opod,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// ---- response types (non-streaming) ----

type anthropicResponse struct {
	ID           string             `json:"id"`
	Type         string             `json:"type"`
	Role         string             `json:"role"`
	Content      []anthropicContent `json:"content"`
	Model        string             `json:"model"`
	StopReason   string             `json:"stop_reason"`
	StopSequence *string            `json:"stop_sequence"`
	Usage        anthropicUsage     `json:"usage"`
}

type anthropicContent struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`       // tool_use payload
	ToolUseID string                `json:"tool_use_id,omitempty"` // tool_result link
	Content   json.RawMessage       `json:"content,omitempty"`     // tool_result body (string or array)
	Source    *anthropicImageSource `json:"source,omitempty"`      // image block
}

// anthropicImageSource matches both the {type:"base64", media_type, data}
// and {type:"url", url} shapes from Anthropic's vision API.
type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"` // base64 (no data-URL prefix)
	URL       string `json:"url,omitempty"`
}

type anthropicUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// ---- POST /v1/messages ----

// Messages handles the Anthropic Messages API endpoint.
func (h *AnthropicHandler) Messages(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	// Read the body so pre-call guardrails can inspect (or rewrite)
	// it before we decode — same hook as the OpenAI handler, but the
	// block response is written in the Anthropic error envelope. A
	// rewrite replaces the whole body, so the rewritten Anthropic
	// messages content is what gets decoded below.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	rewritten, blockedBy, reason := evalPreCallGuardrails(r.Context(), h.Store, body)
	if blockedBy != "" {
		writeAnthropicGuardrailBlocked(w, blockedBy, reason)
		return
	}

	var req anthropicRequest
	if err := json.Unmarshal(rewritten, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if len(req.Messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "messages cannot be empty")
		return
	}
	if req.MaxTokens <= 0 {
		req.MaxTokens = 4096
	}

	// Strip an OpenRouter-style `:floor` / `:nitro` routing suffix before
	// model resolution — it's a routing hint, not part of the model id.
	requested, sortHint := models.SplitSortSuffix(req.Model)
	if requested == "" {
		requested = h.Default
	}
	// Re-check the per-key allowlist on the post-substitution model.
	// ModelAllowMiddleware passes empty-model requests through, so the
	// default must be authorized for this key too.
	if !modelAllowedForKey(r.Context(), requested) {
		key := auth.KeyFrom(r.Context())
		auditRefusal(r.Context(), h.Store, key, requested)
		writeModelNotAllowed(w, requested, key.AllowedModels)
		return
	}
	resolved, err := h.ResolveModel(requested)
	if err != nil {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error", err.Error())
		return
	}

	system := parseSystem(req.System)
	engineMsgs := anthropicMessagesToEngine(req.Messages)

	engineReq := engines.ChatRequest{
		Model:       resolved,
		System:      system,
		Messages:    engineMsgs,
		Temperature: req.Temperature,
		TopP:        req.TopP,
		MaxTokens:   &req.MaxTokens,
		Stop:        req.StopSeq,
		Stream:      true,
	}

	// Per-request overrides (opod.* body block, X-Opod-* headers, or a
	// :floor/:nitro model suffix).
	ctx := overridesContextAnthropic(r, req.Opod, h.Store, requested, sortHint)

	start := time.Now()
	stream, err := h.Engine.Chat(ctx, engineReq)
	if err != nil {
		recordUsage(r.Context(), h.Store, "anthropic", requested, nil, time.Since(start), "error")
		writeAnthropicError(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}

	msgID := "msg_" + randID()

	if req.Stream {
		h.streamAnthropic(w, r, stream, msgID, requested, start)
		return
	}
	h.aggregateAnthropic(w, r, stream, msgID, requested, start)
}

// ---- POST /v1/messages/count_tokens ----

type countTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// CountTokens is a best-effort token counter (chars / 4) — the real engine
// tokenizer is not exposed by Ollama. Sufficient for client pre-flight checks.
func (h *AnthropicHandler) CountTokens(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req anthropicRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "Invalid JSON")
		return
	}
	total := 0
	if sys := parseSystem(req.System); sys != "" {
		total += len(sys) / 4
	}
	for _, m := range req.Messages {
		total += rawContentChars(m.Content) / 4
	}
	writeJSON(w, http.StatusOK, countTokensResponse{InputTokens: total + 4})
}

// ---- streaming ----

func (h *AnthropicHandler) streamAnthropic(w http.ResponseWriter, r *http.Request,
	stream <-chan engines.StreamEvent, msgID, modelOut string, start time.Time) {
	defer drainStream(stream)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	// message_start
	sendAnthropicEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         modelOut,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})

	// content_block_start
	sendAnthropicEvent(w, flusher, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})

	stopReason := "end_turn"
	outputTokens := 0
	inputTokens := 0

	var finalUsage *engines.Usage
	for ev := range stream {
		if r.Context().Err() != nil {
			recordUsage(r.Context(), h.Store, "anthropic", modelOut, nil, time.Since(start), "cancelled")
			return
		}
		if ev.Err != nil {
			recordUsage(r.Context(), h.Store, "anthropic", modelOut, nil, time.Since(start), "error")
			sendAnthropicEvent(w, flusher, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": ev.Err.Error()},
			})
			return
		}
		if ev.Done {
			finalUsage = ev.Usage
			if ev.Usage != nil {
				outputTokens = ev.Usage.CompletionTokens
				inputTokens = ev.Usage.PromptTokens
			}
			if ev.Reason == "length" {
				stopReason = "max_tokens"
			}
			break
		}
		if ev.Delta != "" {
			sendAnthropicEvent(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 0,
				"delta": map[string]any{"type": "text_delta", "text": ev.Delta},
			})
		}
		if r.Context().Err() != nil {
			return
		}
	}

	// content_block_stop
	sendAnthropicEvent(w, flusher, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	})

	// message_delta with stop_reason + usage
	sendAnthropicEvent(w, flusher, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]int{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	})

	// message_stop
	sendAnthropicEvent(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
	recordUsage(r.Context(), h.Store, "anthropic", modelOut, finalUsage, time.Since(start), "ok")
}

func (h *AnthropicHandler) aggregateAnthropic(w http.ResponseWriter, r *http.Request,
	stream <-chan engines.StreamEvent, msgID, modelOut string, start time.Time) {
	defer drainStream(stream)

	var text strings.Builder
	stopReason := "end_turn"
	var u *engines.Usage
	for ev := range stream {
		if ev.Err != nil {
			recordUsage(r.Context(), h.Store, "anthropic", modelOut, nil, time.Since(start), "error")
			writeAnthropicError(w, http.StatusBadGateway, "api_error", ev.Err.Error())
			return
		}
		if ev.Done {
			u = ev.Usage
			if ev.Reason == "length" {
				stopReason = "max_tokens"
			}
			break
		}
		text.WriteString(ev.Delta)
	}

	resp := anthropicResponse{
		ID:         msgID,
		Type:       "message",
		Role:       "assistant",
		Model:      modelOut,
		StopReason: stopReason,
		Content: []anthropicContent{
			{Type: "text", Text: text.String()},
		},
	}
	if u != nil {
		resp.Usage = anthropicUsage{
			InputTokens:  u.PromptTokens,
			OutputTokens: u.CompletionTokens,
		}
	}
	writeJSON(w, http.StatusOK, resp)
	recordUsage(r.Context(), h.Store, "anthropic", modelOut, u, time.Since(start), "ok")
}

// ---- helpers ----

// parseSystem accepts either a JSON string or a JSON array of content blocks
// (Anthropic accepts both) and returns the concatenated text.
func parseSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []anthropicContent
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var b strings.Builder
		for _, blk := range blocks {
			if blk.Type == "text" {
				b.WriteString(blk.Text)
			}
		}
		return b.String()
	}
	return ""
}

// anthropicMessagesToEngine converts the Anthropic messages list to the
// engine-native list. Content blocks are flattened to a tag-marked text form
// that preserves tool_use Input arguments and tool_result Content payloads so
// the engine model has enough context to continue a tool-using conversation.
//
// We use XML-ish tags rather than the literal JSON because most open models
// understand "<tool_use name=\"foo\">{...}</tool_use>" patterns from their
// training data. Anthropic's native format is not directly speakable by
// vLLM/MLX/Ollama, so this is the lossy-but-honest translation.
func anthropicMessagesToEngine(in []anthropicMessage) []engines.Message {
	out := make([]engines.Message, 0, len(in))
	for _, m := range in {
		// content can be string or array of content blocks
		var asStr string
		if err := json.Unmarshal(m.Content, &asStr); err == nil {
			out = append(out, engines.Message{Role: m.Role, Content: asStr})
			continue
		}
		var blocks []anthropicContent
		if err := json.Unmarshal(m.Content, &blocks); err == nil {
			var b strings.Builder
			var images []string
			for _, blk := range blocks {
				switch blk.Type {
				case "text":
					b.WriteString(blk.Text)
				case "image":
					// Anthropic shape:
					//   {"type":"image","source":{"type":"base64","media_type":"image/png","data":"…"}}
					//   {"type":"image","source":{"type":"url","url":"https://…"}}
					// Both forms flatten to the engine's Images field. Ollama
					// and vLLM accept either base64 (no data-URL prefix) or URL.
					if blk.Source != nil {
						switch blk.Source.Type {
						case "base64":
							if blk.Source.Data != "" {
								images = append(images, blk.Source.Data)
							}
						case "url":
							if blk.Source.URL != "" {
								images = append(images, blk.Source.URL)
							}
						}
					}
				case "tool_use":
					input := "{}"
					if len(blk.Input) > 0 {
						input = string(blk.Input)
					}
					b.WriteString(fmt.Sprintf(
						"\n<tool_use id=%q name=%q>%s</tool_use>\n",
						blk.ID, blk.Name, input))
				case "tool_result":
					content := ""
					if len(blk.Content) > 0 {
						// Content is either a JSON string or array of blocks.
						// Try string first so we don't include quotes.
						var s string
						if err := json.Unmarshal(blk.Content, &s); err == nil {
							content = s
						} else {
							content = string(blk.Content)
						}
					}
					b.WriteString(fmt.Sprintf(
						"\n<tool_result tool_use_id=%q>%s</tool_result>\n",
						blk.ToolUseID, content))
				}
			}
			out = append(out, engines.Message{Role: m.Role, Content: b.String(), Images: images})
			continue
		}
		// Fallback: include as raw string
		out = append(out, engines.Message{Role: m.Role, Content: string(m.Content)})
	}
	return out
}

func rawContentChars(raw json.RawMessage) int {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return len(s)
	}
	return len(raw)
}

func sendAnthropicEvent(w io.Writer, flusher http.Flusher, eventType string, payload any) {
	b, _ := json.Marshal(payload)
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(b))
	if flusher != nil {
		flusher.Flush()
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": msg,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}
