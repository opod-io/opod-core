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
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
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

// ConnectTimeout bounds how long a connection to the engine (or to a worker: a
// leader reaches its workers through this client) may take to be ESTABLISHED.
// It is not a request deadline — responses stream for as long as they take. A
// server on the same machine or the same cluster network accepts in
// milliseconds; one that has not in a few seconds is gone. Go's default
// transport waits 30 s, and an address that no longer exists (a removed pod)
// does not refuse, it hangs: a request picked for such a worker stalled for
// 20–30 s before the router could ask the next worker.
const ConnectTimeout = 3 * time.Second

// streamingHTTPClient has no overall deadline and a bounded connect. Everything
// else is Go's default transport.
func streamingHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: ConnectTimeout, KeepAlive: 30 * time.Second}).DialContext
	return &http.Client{Timeout: 0, Transport: tr}
}

// NewClient returns a Client for driver at endpoint. auth may be nil.
func NewClient(driver, endpoint string, auth func(*http.Request)) Client {
	return Client{
		Driver:  driver,
		BaseURL: strings.TrimRight(endpoint, "/"),
		HTTP:    streamingHTTPClient(),
		Auth:    auth,
	}
}

// CloseIdleConnections releases the client's pooled sockets. Every driver owns
// a private transport (streamingHTTPClient clones the default one), so an
// engine the router evicts — a removed node, a torn-down gang, a coordinator
// that moved — kept its idle connections and their reader goroutines alive for
// the 90 s idle timeout, unbounded in aggregate while nodes churned (PLAN
// T15.16). The router calls this at every eviction site.
func (c *Client) CloseIdleConnections() {
	if c.HTTP != nil {
		c.HTTP.CloseIdleConnections()
	}
}

// SignAsNode makes every request authenticate as an opod node: the HMAC
// signature the worker prefers, plus the bearer header for one transition
// release (engines.NodeSigned). It replaces whatever Auth the driver was built
// with, because a worker's token is the key, not an upstream API key.
func (c *Client) SignAsNode(nodeID, token string) {
	c.Auth = func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+token) // transition; the signature is the real auth
		auth.SignRequest(req, nodeID, token)
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
	consumeStream(ctx, body, out, nil)
}

// consumeStream is the one loop behind ConsumeStream and
// ConsumeStreamWithSpan: it forwards every event and, when asked, reports the
// final usage to onUsage before the Done event is sent. The span variant used
// to run ConsumeStream in a goroutine behind a second channel only to read
// two integers off the last event — one more hop per token on both sides of
// the leader (~68 ns per token per hop, ~135 µs per 2000-token stream, PLAN
// T15.9). Single close point, as before: every exit path closes out, because
// consumers drain with a bare `for range` and a missed close leaks their
// goroutine permanently.
func consumeStream(ctx context.Context, body io.ReadCloser, out chan<- engines.StreamEvent, onUsage func(*engines.Usage)) {
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
		if onUsage != nil && u != nil {
			onUsage(u)
		}
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

// ConsumeStreamWithSpan is ConsumeStream plus the span: it records the
// per-request token counts and ends the span once the stream is drained or
// the context cancels. Spans are no-op when the global TracerProvider isn't
// set (OPOD_OTLP_ENDPOINT unset), so this path has zero overhead in the
// default config. It runs in the caller's goroutine and starts none.
func ConsumeStreamWithSpan(ctx context.Context, body io.ReadCloser, out chan<- engines.StreamEvent, span *engines.ChatSpan) {
	defer span.End()
	var promptTokens, completionTokens int
	consumeStream(ctx, body, out, func(u *engines.Usage) {
		promptTokens, completionTokens = u.PromptTokens, u.CompletionTokens
	})
	span.SetTokens(promptTokens, completionTokens)
}
