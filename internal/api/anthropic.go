package api

// Anthropic Messages (ADR-092 §1, PLAN T19.3): POST /v1/messages and
// /v1/messages/count_tokens, served by translating to the chat path and back.
// A Messages request becomes a chat request, rides the same admission, router,
// guardrails and usage row as any other, and its answer — aggregated or
// streamed — is rewritten into Anthropic's shapes on the way out. Every engine
// serves it, because no engine sees it.
//
// What is translated: `system`, text and base64 image blocks, `tool_use` and
// `tool_result`, `tools`, `tool_choice` (with `disable_parallel_tool_use`),
// `stop_sequences`, `max_tokens` (required, as Anthropic requires it),
// `temperature`, `top_p`, `top_k` and `metadata.user_id`. What is refused, 400
// in Anthropic's envelope before anything is admitted: extended `thinking`,
// server tools (a tool with a `type` other than `custom`), images by URL or
// file id, `document` and other block kinds the chat path has no form for,
// `mcp_servers`, `container`, `context_management`, and any field this
// translator does not know — Anthropic's own API refuses unknown fields, and
// one accepted here and ignored would be a setting that does nothing.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// ProtocolAnthropic is the usage row's protocol for a Messages request.
const ProtocolAnthropic = "anthropic"

type protocolKey struct{}

// WithProtocol names the wire protocol the caller spoke, for the usage row: a
// Messages request is served by the chat handler, which records "openai"
// unless told otherwise.
func WithProtocol(ctx context.Context, protocol string) context.Context {
	return context.WithValue(ctx, protocolKey{}, protocol)
}

func protocolOf(ctx context.Context, fallback string) string {
	if p, ok := ctx.Value(protocolKey{}).(string); ok && p != "" {
		return p
	}
	return fallback
}

// WithRequestBody makes body the request's body for every reader after this
// point — the handler and anything that reads the stash requestBody keeps. A
// translated request must replace the original bytes there: a middleware that
// read the original (the per-key limits read `model`) stashed it, and the chat
// handler reads the stash first.
func WithRequestBody(r *http.Request, body []byte) *http.Request {
	r = r.WithContext(context.WithValue(r.Context(), bodyKey{}, &bufferedBody{bytes: body}))
	r.Body = io.NopCloser(bytes.NewReader(body))
	return r
}

// AnthropicError is a refusal in Anthropic's envelope.
type AnthropicError struct {
	Status  int
	Type    string
	Message string
}

func (e *AnthropicError) Error() string { return e.Message }

func invalidAnthropic(format string, args ...any) *AnthropicError {
	return &AnthropicError{Status: http.StatusBadRequest, Type: "invalid_request_error", Message: fmt.Sprintf(format, args...)}
}

// WriteAnthropicError writes e as Anthropic does: {"type":"error","error":{…}}.
func WriteAnthropicError(w http.ResponseWriter, e *AnthropicError) {
	w.Header().Del("Content-Length")
	writeJSON(w, e.Status, map[string]any{"type": "error", "error": map[string]any{"type": e.Type, "message": e.Message}})
}

// anthropicErrorType is Anthropic's error type for an HTTP status.
func anthropicErrorType(status int) string {
	switch {
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusForbidden:
		return "permission_error"
	case status == http.StatusNotFound:
		return "not_found_error"
	case status == http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusServiceUnavailable:
		return "overloaded_error"
	case status >= 500:
		return "api_error"
	default:
		return "invalid_request_error"
	}
}

// ---- request: Messages → chat ----

// anthropicKeys are the top-level fields a Messages request may carry here.
// `metadata` and `service_tier` are accepted: the first maps to `user`, the
// second has no meaning on a private endpoint and changes nothing a caller
// could observe.
var anthropicKeys = map[string]bool{
	"model": true, "messages": true, "system": true, "max_tokens": true, "stop_sequences": true, "stream": true,
	"temperature": true, "top_p": true, "top_k": true, "tools": true, "tool_choice": true, "metadata": true,
	"thinking": true, "service_tier": true,
}

// anthropicRefused are fields Anthropic serves and this endpoint cannot.
var anthropicRefused = map[string]string{
	"mcp_servers":        "MCP servers are run by Anthropic's API, not by a model endpoint",
	"container":          "code-execution containers are run by Anthropic's API, not by a model endpoint",
	"context_management": "context editing is run by Anthropic's API, not by a model endpoint",
}

type anthropicRequest struct {
	Model         string             `json:"model"`
	Messages      []anthropicMessage `json:"messages"`
	System        json.RawMessage    `json:"system"`
	MaxTokens     *int               `json:"max_tokens"`
	StopSequences []string           `json:"stop_sequences"`
	Stream        bool               `json:"stream"`
	Temperature   *float64           `json:"temperature"`
	TopP          *float64           `json:"top_p"`
	TopK          *int               `json:"top_k"`
	Tools         []anthropicTool    `json:"tools"`
	ToolChoice    *struct {
		Type                   string `json:"type"`
		Name                   string `json:"name"`
		DisableParallelToolUse bool   `json:"disable_parallel_tool_use"`
	} `json:"tool_choice"`
	Metadata *struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
	Thinking *struct {
		Type string `json:"type"`
	} `json:"thinking"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Source    *anthropicImage `json:"source"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

type anthropicImage struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// AnthropicToChat translates a Messages request into the chat body the chat
// handler serves, and says which model it names and whether it streams. For
// count_tokens (count) max_tokens is not required and the chat request is a
// one-token, unstreamed generation: the engine's own prompt count is the
// answer, with its chat template and the tools in it.
func AnthropicToChat(body []byte, count bool) (chat []byte, model string, stream bool, aerr *AnthropicError) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		return nil, "", false, invalidAnthropic("invalid JSON body: %v", err)
	}
	for k := range top {
		if why, refused := anthropicRefused[k]; refused {
			return nil, "", false, invalidAnthropic("%s: not supported here — %s", k, why)
		}
		if !anthropicKeys[k] {
			return nil, "", false, invalidAnthropic("%s: Extra inputs are not permitted", k)
		}
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, "", false, invalidAnthropic("invalid JSON body: %v", err)
	}
	if req.Thinking != nil && req.Thinking.Type != "" && req.Thinking.Type != "disabled" {
		return nil, "", false, invalidAnthropic("thinking: extended thinking is not supported on this endpoint yet — leave it out or set {\"type\":\"disabled\"}")
	}
	if len(req.Messages) == 0 {
		return nil, "", false, invalidAnthropic("messages: at least one message is required")
	}
	if !count && req.MaxTokens == nil {
		return nil, "", false, invalidAnthropic("max_tokens: Field required")
	}

	out := map[string]any{"model": req.Model}
	var msgs []map[string]any
	if len(req.System) > 0 && string(req.System) != "null" {
		sys, err := systemText(req.System)
		if err != nil {
			return nil, "", false, err
		}
		if sys != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": sys})
		}
	}
	for i, m := range req.Messages {
		turn, err := chatTurns(i, m)
		if err != nil {
			return nil, "", false, err
		}
		msgs = append(msgs, turn...)
	}
	out["messages"] = msgs

	if count {
		out["max_tokens"] = 1
	} else {
		out["max_tokens"] = *req.MaxTokens
		out["stream"] = req.Stream
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.TopK != nil {
		out["top_k"] = *req.TopK
	}
	if req.Metadata != nil && req.Metadata.UserID != "" {
		out["user"] = req.Metadata.UserID
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			if t.Type != "" && t.Type != "custom" {
				return nil, "", false, invalidAnthropic("tools: %q is a server tool (type %s), which Anthropic's API runs — only custom tools with an input_schema are served here", t.Name, t.Type)
			}
			if t.Name == "" || len(t.InputSchema) == 0 {
				return nil, "", false, invalidAnthropic("tools: each tool needs a name and an input_schema")
			}
			fn := map[string]any{"name": t.Name, "parameters": t.InputSchema}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			tools = append(tools, map[string]any{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}
	if tc := req.ToolChoice; tc != nil {
		switch tc.Type {
		case "auto", "":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "none":
			out["tool_choice"] = "none"
		case "tool":
			if tc.Name == "" {
				return nil, "", false, invalidAnthropic("tool_choice: type tool needs a name")
			}
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": tc.Name}}
		default:
			return nil, "", false, invalidAnthropic("tool_choice: unknown type %q", tc.Type)
		}
		if tc.DisableParallelToolUse {
			out["parallel_tool_calls"] = false
		}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, "", false, invalidAnthropic("%v", err)
	}
	return raw, req.Model, req.Stream && !count, nil
}

// systemText is `system` as one string: a string, or text blocks joined.
func systemText(raw json.RawMessage) (string, *AnthropicError) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", invalidAnthropic("system: a string or a list of text blocks")
	}
	parts := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if b.Type != "text" {
			return "", invalidAnthropic("system: only text blocks are allowed, got %q", b.Type)
		}
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n\n"), nil
}

// chatTurns is one Messages turn as chat messages. A user turn's tool results
// become `tool` messages, first and in order, as the chat wire requires them
// right after the assistant's calls; the rest of the turn follows as one user
// message. An assistant turn is one message with its text and its calls.
func chatTurns(i int, m anthropicMessage) ([]map[string]any, *AnthropicError) {
	if m.Role != "user" && m.Role != "assistant" {
		return nil, invalidAnthropic("messages.%d.role: must be user or assistant, got %q", i, m.Role)
	}
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return []map[string]any{{"role": m.Role, "content": s}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(m.Content, &blocks); err != nil {
		return nil, invalidAnthropic("messages.%d.content: a string or a list of content blocks", i)
	}
	var out []map[string]any
	var parts []map[string]any
	var calls []map[string]any
	hasImage := false
	for j, b := range blocks {
		at := fmt.Sprintf("messages.%d.content.%d", i, j)
		switch b.Type {
		case "text":
			parts = append(parts, map[string]any{"type": "text", "text": b.Text})
		case "image":
			if m.Role != "user" {
				return nil, invalidAnthropic("%s: an image is allowed in a user turn only", at)
			}
			url, err := imageDataURL(at, b.Source)
			if err != nil {
				return nil, err
			}
			hasImage = true
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
		case "tool_use":
			if m.Role != "assistant" {
				return nil, invalidAnthropic("%s: tool_use is allowed in an assistant turn only", at)
			}
			args := "{}"
			if len(b.Input) > 0 {
				args = string(b.Input)
			}
			calls = append(calls, map[string]any{"id": b.ID, "type": "function", "function": map[string]any{"name": b.Name, "arguments": args}})
		case "tool_result":
			if m.Role != "user" {
				return nil, invalidAnthropic("%s: tool_result is allowed in a user turn only", at)
			}
			text, err := toolResultText(at, b)
			if err != nil {
				return nil, err
			}
			out = append(out, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": text})
		default:
			return nil, invalidAnthropic("%s: content blocks of type %q are not supported here (text, image, tool_use and tool_result are)", at, b.Type)
		}
	}
	if m.Role == "assistant" {
		msg := map[string]any{"role": "assistant", "content": joinText(parts)}
		if len(calls) > 0 {
			msg["tool_calls"] = calls
		}
		return append(out, msg), nil
	}
	if len(parts) > 0 {
		if hasImage {
			out = append(out, map[string]any{"role": "user", "content": parts})
		} else {
			out = append(out, map[string]any{"role": "user", "content": joinText(parts)})
		}
	}
	return out, nil
}

func joinText(parts []map[string]any) string {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		if t, ok := p["text"].(string); ok {
			texts = append(texts, t)
		}
	}
	return strings.Join(texts, "\n\n")
}

// imageDataURL is a base64 image block as the data URL the chat path takes.
func imageDataURL(at string, src *anthropicImage) (string, *AnthropicError) {
	if src == nil || src.Type != "base64" {
		kind := "none"
		if src != nil {
			kind = src.Type
		}
		return "", invalidAnthropic("%s.source: only base64 images are served here (got %s); send the bytes, not a URL or a file id", at, kind)
	}
	if src.MediaType == "" || src.Data == "" {
		return "", invalidAnthropic("%s.source: media_type and data are required", at)
	}
	return "data:" + src.MediaType + ";base64," + src.Data, nil
}

// toolResultText is a tool_result's content as the text a `tool` message
// carries. An error result keeps its text; the model reads the words.
func toolResultText(at string, b anthropicBlock) (string, *AnthropicError) {
	if len(b.Content) == 0 || string(b.Content) == "null" {
		return "", nil
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(b.Content, &blocks); err != nil {
		return "", invalidAnthropic("%s.content: a string or a list of text blocks", at)
	}
	texts := make([]string, 0, len(blocks))
	for _, c := range blocks {
		if c.Type != "text" {
			return "", invalidAnthropic("%s.content: only text is served inside a tool result here, got %q", at, c.Type)
		}
		texts = append(texts, c.Text)
	}
	return strings.Join(texts, "\n\n"), nil
}

// ---- response: chat → Messages ----

// anthropicStopReason is a chat finish_reason as Anthropic's stop_reason.
func anthropicStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	default:
		return "end_turn"
	}
}

// toolInput is a call's arguments as the object Anthropic's `input` is. An
// engine whose arguments are not a JSON object has its text kept under
// `arguments` rather than lost.
func toolInput(args string) json.RawMessage {
	trimmed := strings.TrimSpace(args)
	if trimmed == "" {
		return json.RawMessage("{}")
	}
	if strings.HasPrefix(trimmed, "{") && json.Valid([]byte(trimmed)) {
		return json.RawMessage(trimmed)
	}
	raw, _ := json.Marshal(map[string]string{"arguments": args})
	return raw
}

type chatToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// anthropicMessageFrom translates an aggregated chat answer.
func anthropicMessageFrom(raw []byte, model string) ([]byte, error) {
	var resp struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   json.RawMessage `json:"content"`
				ToolCalls []chatToolCall  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage usage `json:"usage"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	content := []map[string]any{}
	finish := ""
	if len(resp.Choices) > 0 {
		c := resp.Choices[0]
		finish = c.FinishReason
		var text string
		if json.Unmarshal(c.Message.Content, &text) == nil && text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		}
		for _, tc := range c.Message.ToolCalls {
			id := tc.ID
			if id == "" {
				id = "toolu_" + randID()
			}
			content = append(content, map[string]any{"type": "tool_use", "id": id, "name": tc.Function.Name, "input": toolInput(tc.Function.Arguments)})
		}
	}
	if resp.Model != "" {
		model = resp.Model
	}
	return json.Marshal(map[string]any{
		"id": "msg_" + randID(), "type": "message", "role": "assistant", "model": model, "content": content,
		"stop_reason": anthropicStopReason(finish), "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": resp.Usage.PromptTokens, "output_tokens": resp.Usage.CompletionTokens},
	})
}

// AnthropicWriter sits between the chat handler and the caller and rewrites
// what the handler writes: an error into Anthropic's envelope, an aggregated
// answer into a Message, and a chat stream into Anthropic's events as each
// chunk arrives (message_start, content_block_start / _delta / _stop for each
// text or tool_use block, message_delta, message_stop; an error mid-stream is
// an `error` event). Headers the handler sets — X-Request-Id, X-Opod-* — pass
// through. Finish must be called once the handler returns.
type AnthropicWriter struct {
	w      http.ResponseWriter
	flush  http.Flusher
	model  string
	status int
	header bool // the handler has written its status
	stream bool // the handler answers with an event stream
	buf    bytes.Buffer

	// stream state
	started   bool
	finished  bool
	open      int    // the open content block's index, -1 when none
	openKind  string // "text" | "tool_use"
	next      int    // the next block index
	toolBlock map[int]int
}

// NewAnthropicWriter wraps w for a Messages request naming model.
func NewAnthropicWriter(w http.ResponseWriter, model string) *AnthropicWriter {
	f, _ := w.(http.Flusher)
	return &AnthropicWriter{w: w, flush: f, model: model, status: http.StatusOK, open: -1, toolBlock: map[int]int{}}
}

func (a *AnthropicWriter) Header() http.Header { return a.w.Header() }

func (a *AnthropicWriter) WriteHeader(code int) {
	if a.header {
		return
	}
	a.header, a.status = true, code
	a.stream = code < 300 && strings.HasPrefix(a.w.Header().Get("Content-Type"), "text/event-stream")
	if a.stream {
		a.w.Header().Del("Content-Length")
		a.w.WriteHeader(code)
	}
}

func (a *AnthropicWriter) Write(p []byte) (int, error) {
	if !a.header {
		a.WriteHeader(http.StatusOK)
	}
	a.buf.Write(p)
	if a.stream {
		a.drain()
	}
	return len(p), nil
}

// Flush passes through on a stream; a buffered answer is written by Finish.
func (a *AnthropicWriter) Flush() {
	if a.stream && a.flush != nil {
		a.flush.Flush()
	}
}

// Finish writes what was buffered: the translated error or Message.
func (a *AnthropicWriter) Finish() {
	if a.stream {
		return
	}
	raw := a.buf.Bytes()
	if a.status >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		msg := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			msg = e.Error.Message
		}
		WriteAnthropicError(a.w, &AnthropicError{Status: a.status, Type: anthropicErrorType(a.status), Message: msg})
		return
	}
	out, err := anthropicMessageFrom(raw, a.model)
	if err != nil {
		WriteAnthropicError(a.w, &AnthropicError{Status: http.StatusBadGateway, Type: "api_error", Message: "the answer could not be translated: " + err.Error()})
		return
	}
	a.w.Header().Del("Content-Length")
	a.w.Header().Set("Content-Type", "application/json")
	a.w.WriteHeader(a.status)
	_, _ = a.w.Write(out)
}

// drain translates every complete SSE event in the buffer.
func (a *AnthropicWriter) drain() {
	for {
		raw := a.buf.Bytes()
		end := bytes.Index(raw, []byte("\n\n"))
		if end < 0 {
			return
		}
		event := string(raw[:end])
		a.buf.Next(end + 2)
		for _, line := range strings.Split(event, "\n") {
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				a.chunk(data)
			}
		}
	}
}

func (a *AnthropicWriter) emit(typ string, v map[string]any) {
	v["type"] = typ
	b, _ := json.Marshal(v)
	_, _ = fmt.Fprintf(a.w, "event: %s\ndata: %s\n\n", typ, b)
}

// chunk translates one chat stream chunk.
func (a *AnthropicWriter) chunk(data string) {
	if a.finished || data == "[DONE]" {
		return
	}
	var c struct {
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content   *string        `json:"content"`
				ToolCalls []chatToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *usage `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(data), &c); err != nil {
		return
	}
	if c.Error != nil {
		a.emit("error", map[string]any{"error": map[string]any{"type": "api_error", "message": c.Error.Message}})
		a.finished = true
		return
	}
	if !a.started {
		a.started = true
		model := a.model
		if c.Model != "" {
			model = c.Model
		}
		a.emit("message_start", map[string]any{"message": map[string]any{
			"id": "msg_" + randID(), "type": "message", "role": "assistant", "model": model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		}})
		a.emit("ping", map[string]any{})
	}
	if len(c.Choices) == 0 {
		return
	}
	ch := c.Choices[0]
	if t := ch.Delta.Content; t != nil && *t != "" {
		if a.openKind != "text" {
			a.closeBlock()
			a.openBlock("text", map[string]any{"type": "text", "text": ""})
		}
		a.emit("content_block_delta", map[string]any{"index": a.open, "delta": map[string]any{"type": "text_delta", "text": *t}})
	}
	for k, tc := range ch.Delta.ToolCalls {
		idx := k
		if tc.Index != nil {
			idx = *tc.Index
		}
		block, seen := a.toolBlock[idx]
		if !seen || block != a.open {
			if !seen {
				a.closeBlock()
				id := tc.ID
				if id == "" {
					id = "toolu_" + randID()
				}
				a.openBlock("tool_use", map[string]any{"type": "tool_use", "id": id, "name": tc.Function.Name, "input": map[string]any{}})
				a.toolBlock[idx] = a.open
			} else {
				// A fragment of a call whose block was closed by a later one:
				// the chat wire allows it, Anthropic's does not; it is dropped
				// rather than written into the wrong block.
				continue
			}
		}
		if tc.Function.Arguments != "" {
			a.emit("content_block_delta", map[string]any{"index": a.open, "delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments}})
		}
	}
	if ch.FinishReason != nil {
		a.closeBlock()
		u := map[string]int{"output_tokens": 0}
		if c.Usage != nil {
			u["input_tokens"], u["output_tokens"] = c.Usage.PromptTokens, c.Usage.CompletionTokens
		}
		a.emit("message_delta", map[string]any{"delta": map[string]any{"stop_reason": anthropicStopReason(*ch.FinishReason), "stop_sequence": nil}, "usage": u})
		a.emit("message_stop", map[string]any{})
		a.finished = true
	}
}

func (a *AnthropicWriter) openBlock(kind string, block map[string]any) {
	a.open, a.openKind = a.next, kind
	a.next++
	a.emit("content_block_start", map[string]any{"index": a.open, "content_block": block})
}

func (a *AnthropicWriter) closeBlock() {
	if a.open < 0 {
		return
	}
	a.emit("content_block_stop", map[string]any{"index": a.open})
	a.open, a.openKind = -1, ""
}
