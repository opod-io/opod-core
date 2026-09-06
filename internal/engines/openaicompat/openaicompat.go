// Package openaicompat holds what every OpenAI-wire-compatible driver shares
// — vLLM, MLX-LM, llama-server, Tenstorrent's tt-metal server: request body
// shaping, SSE stream decoding, and the Health/List/Chat request skeletons.
// A driver package embeds Client and adds only what differs (its name, auth,
// a different health route, pull semantics). It never re-implements the wire.
package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/opod-io/opod/internal/engines"
)

// Client is the reusable OpenAI-compatible HTTP core a driver embeds.
type Client struct {
	// Driver is the canonical engine name, used in errors and span names.
	Driver string
	// BaseURL is the server root without a trailing slash (e.g. http://gpu:8000).
	BaseURL string
	// HTTP performs requests. Streaming — no overall deadline.
	HTTP *http.Client
	// Auth, when non-nil, decorates every request (Bearer token etc.).
	Auth func(*http.Request)
}

// NewClient returns a Client for driver at endpoint. auth may be nil.
func NewClient(driver, endpoint string, auth func(*http.Request)) Client {
	return Client{
		Driver:  driver,
		BaseURL: strings.TrimRight(endpoint, "/"),
		HTTP:    &http.Client{Timeout: 0},
		Auth:    auth,
	}
}

// Endpoint satisfies engines.Engine.
func (c Client) Endpoint() string { return c.BaseURL }

func (c Client) applyAuth(req *http.Request) {
	if c.Auth != nil {
		c.Auth(req)
	}
}

// Health returns nil when GET /v1/models answers 200.
func (c Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/models", nil)
	if err != nil {
		return err
	}
	c.applyAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return engines.Unreachable(c.Driver, c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return engines.Upstream(c.Driver, "GET /v1/models", resp.StatusCode, nil)
	}
	return nil
}

// List returns the model ids GET /v1/models advertises.
func (c Client) List(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	c.applyAuth(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, engines.Unreachable(c.Driver, c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, engines.Upstream(c.Driver, "list models", resp.StatusCode, b)
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("%s list models: decode: %w", c.Driver, err)
	}
	out := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		out = append(out, m.ID)
	}
	return out, nil
}

// Chat POSTs an OpenAI chat completion and adapts the streamed SSE response
// into Opod's StreamEvent channel. Synchronous failures return an error and
// a nil channel; once a channel is returned it is always closed.
func (c Client) Chat(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	ctx, span := engines.StartChatSpan(ctx, c.Driver, req.Model, c.BaseURL, len(req.Messages))
	// span.End() runs in ConsumeStreamWithSpan so the duration covers the
	// full streamed response, not just stream-start.

	out := make(chan engines.StreamEvent, 16)
	raw, _ := json.Marshal(BuildChatBody(req))

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		span.MarkError("new request", err)
		span.End()
		close(out)
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	c.applyAuth(httpReq)

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		span.MarkError("http do", err)
		span.End()
		close(out)
		return nil, engines.Unreachable(c.Driver, c.BaseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		span.SetHTTPStatus(resp.StatusCode)
		span.MarkError(resp.Status, nil)
		span.End()
		close(out)
		return nil, engines.Upstream(c.Driver, "chat", resp.StatusCode, b)
	}
	span.SetHTTPStatus(resp.StatusCode)

	go ConsumeStreamWithSpan(ctx, resp.Body, out, span)
	return out, nil
}

// NativeName is the OpenAI-family naming rule: these servers load a
// Hugging Face repo or a local weights path; the catalog id is only an alias.
func NativeName(src engines.Source) string {
	if src.Repo != "" {
		return src.Repo
	}
	return src.Path
}

// BuildChatBody shapes an engines.ChatRequest as an OpenAI chat body.
func BuildChatBody(req engines.ChatRequest) map[string]any {
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		body["max_tokens"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 {
		body["stop"] = req.Stop
	}
	if req.Stream {
		body["stream_options"] = map[string]bool{"include_usage": true}
	}
	return body
}

// ConsumeStream reads an SSE response from an OpenAI-compatible server and
// translates each chunk into a StreamEvent. It correctly handles servers
// (vLLM, MLX) that send a separate usage-only chunk AFTER the finish_reason
// chunk: the function captures finish_reason but continues reading until
// it sees `[DONE]` or a chunk carrying Usage. It closes body and out.
func ConsumeStream(ctx context.Context, body io.ReadCloser, out chan<- engines.StreamEvent) {
	defer body.Close()
	defer close(out)
	send := func(ev engines.StreamEvent) bool {
		select {
		case out <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	sc := bufio.NewScanner(body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var finishReason string
	var emittedFinal bool

	emitFinal := func(u *engines.Usage) {
		if emittedFinal {
			return
		}
		emittedFinal = true
		send(engines.StreamEvent{Done: true, Reason: finishReason, Usage: u})
	}

	for sc.Scan() {
		if ctx.Err() != nil {
			return
		}
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			emitFinal(nil)
			return
		}
		var ev struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			send(engines.StreamEvent{Err: fmt.Errorf("decode openai chunk: %w", err)})
			return
		}
		if len(ev.Choices) > 0 {
			ch := ev.Choices[0]
			if ch.Delta.Content != "" {
				if !send(engines.StreamEvent{Delta: ch.Delta.Content}) {
					return
				}
			}
			if ch.FinishReason != nil {
				finishReason = *ch.FinishReason
				// Don't return — wait for the usage-only chunk or [DONE].
				// If the same chunk carries Usage, emit final immediately.
				if ev.Usage != nil {
					emitFinal(&engines.Usage{
						PromptTokens:     ev.Usage.PromptTokens,
						CompletionTokens: ev.Usage.CompletionTokens,
						TotalTokens:      ev.Usage.TotalTokens,
					})
					return
				}
			}
		}
		if ev.Usage != nil && len(ev.Choices) == 0 {
			emitFinal(&engines.Usage{
				PromptTokens:     ev.Usage.PromptTokens,
				CompletionTokens: ev.Usage.CompletionTokens,
				TotalTokens:      ev.Usage.TotalTokens,
			})
			return
		}
	}
	if err := sc.Err(); err != nil {
		send(engines.StreamEvent{Err: fmt.Errorf("stream: %w", err)})
		return
	}
	// stream ended without explicit [DONE] — synthesize one
	emitFinal(nil)
}

// ConsumeStreamWithSpan wraps ConsumeStream and additionally records the
// per-request token counts + closes the span once the stream is drained or
// the context cancels. Spans are no-op when the global TracerProvider isn't
// set (OPOD_OTLP_ENDPOINT unset), so this path has zero overhead in the
// default config.
func ConsumeStreamWithSpan(ctx context.Context, body io.ReadCloser, out chan<- engines.StreamEvent, span *engines.ChatSpan) {
	// Single close point: every exit path (drained, ctx cancelled) must
	// close `out` — consumers drain with a bare `for range`, so a missed
	// close leaks their goroutine permanently.
	defer span.End()
	defer close(out)
	intermediate := make(chan engines.StreamEvent, 16)
	go ConsumeStream(ctx, body, intermediate)
	var promptTokens, completionTokens int
	for ev := range intermediate {
		if ev.Done && ev.Usage != nil {
			promptTokens = ev.Usage.PromptTokens
			completionTokens = ev.Usage.CompletionTokens
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			// drain so the upstream producer doesn't block
			go func() {
				for range intermediate {
				}
			}()
			span.SetTokens(promptTokens, completionTokens)
			return
		}
	}
	span.SetTokens(promptTokens, completionTokens)
}
