// Package ollama is the Ollama driver: HTTP to a local or remote Ollama
// daemon. See https://github.com/ollama/ollama/blob/main/docs/api.md for the
// protocol. Registers as "ollama".
package ollama

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/otel/codes"

	"github.com/opod-io/opod/internal/engines"
)

const name = "ollama"

func init() {
	engines.Register(engines.Descriptor{
		Name:       name,
		New:        func(endpoint, _ string) engines.Engine { return New(endpoint) },
		NativeName: func(src engines.Source) string { return src.OllamaName },
		StartHint:  "start it with: ollama serve",
	})
}

// Driver is an Engine that talks to an Ollama HTTP server.
type Driver struct {
	endpoint string
	client   *http.Client
}

// New returns a driver pointing at endpoint (e.g. http://127.0.0.1:11434).
func New(endpoint string) *Driver {
	return &Driver{
		endpoint: strings.TrimRight(endpoint, "/"),
		client:   &http.Client{Timeout: 0}, // streaming — no overall deadline
	}
}

func (o *Driver) Name() string     { return name }
func (o *Driver) Endpoint() string { return o.endpoint }

// do performs one request, classifying transport failures as
// engines.ErrUnreachable and non-2xx answers as engines.ErrUpstream.
func (o *Driver) do(req *http.Request, op string) (*http.Response, error) {
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, engines.Unreachable(name, o.endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return nil, engines.Upstream(name, op, resp.StatusCode, b)
	}
	return resp, nil
}

func (o *Driver) postJSON(ctx context.Context, path string, payload any) (*http.Request, error) {
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// Health returns nil if Ollama is reachable.
func (o *Driver) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint+"/api/version", nil)
	if err != nil {
		return err
	}
	resp, err := o.do(req, "version")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// List returns the model tags installed in Ollama.
func (o *Driver) List(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint+"/api/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.do(req, "list models")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	out := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		out = append(out, m.Name)
	}
	return out, nil
}

// Pull pulls a model. onProgress is called with intermediate updates (may be nil).
func (o *Driver) Pull(ctx context.Context, modelID string, onProgress func(status string, completed, total int64)) error {
	req, err := o.postJSON(ctx, "/api/pull", map[string]any{"name": modelID, "stream": true})
	if err != nil {
		return err
	}
	resp, err := o.do(req, "pull")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var ev struct {
			Status    string `json:"status"`
			Digest    string `json:"digest,omitempty"`
			Total     int64  `json:"total,omitempty"`
			Completed int64  `json:"completed,omitempty"`
			Error     string `json:"error,omitempty"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Error != "" {
			return fmt.Errorf("ollama pull: %s", ev.Error)
		}
		if onProgress != nil {
			onProgress(ev.Status, ev.Completed, ev.Total)
		}
	}
	return sc.Err()
}

// Embed calls Ollama's POST /api/embed.
//
// Ollama accepts either a single input string or a list; we always send the
// list form for predictability. The response shape is:
//
//	{
//	  "model": "nomic-embed-text",
//	  "embeddings": [[...], [...]],
//	  "prompt_eval_count": 12
//	}
func (o *Driver) Embed(ctx context.Context, req engines.EmbedRequest) (engines.EmbedResponse, error) {
	if len(req.Inputs) == 0 {
		return engines.EmbedResponse{}, fmt.Errorf("embed: at least one input is required")
	}
	httpReq, err := o.postJSON(ctx, "/api/embed", map[string]any{
		"model": req.Model,
		"input": req.Inputs,
	})
	if err != nil {
		return engines.EmbedResponse{}, err
	}
	resp, err := o.do(httpReq, "embed")
	if err != nil {
		return engines.EmbedResponse{}, err
	}
	defer resp.Body.Close()

	var out struct {
		Embeddings      [][]float32 `json:"embeddings"`
		PromptEvalCount int         `json:"prompt_eval_count"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return engines.EmbedResponse{}, fmt.Errorf("decode embed response: %w", err)
	}
	if len(out.Embeddings) != len(req.Inputs) {
		return engines.EmbedResponse{}, fmt.Errorf("ollama embed: expected %d vectors, got %d", len(req.Inputs), len(out.Embeddings))
	}
	return engines.EmbedResponse{
		Vectors: out.Embeddings,
		Usage: &engines.Usage{
			PromptTokens: out.PromptEvalCount,
			TotalTokens:  out.PromptEvalCount,
		},
	}, nil
}

// Resident returns the models currently loaded in Ollama's memory via
// /api/ps, with their actual RAM/VRAM byte footprints. This is the
// ground truth for admission decisions — /api/tags (List) only says
// what's installed on disk.
func (o *Driver) Resident(ctx context.Context) ([]engines.ResidentModel, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.endpoint+"/api/ps", nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.do(req, "ps")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Models []struct {
			Name     string `json:"name"`
			Size     int64  `json:"size"`
			SizeVRAM int64  `json:"size_vram"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode resident models: %w", err)
	}
	out := make([]engines.ResidentModel, 0, len(body.Models))
	for _, m := range body.Models {
		out = append(out, engines.ResidentModel{Name: m.Name, SizeBytes: m.Size, VRAMBytes: m.SizeVRAM})
	}
	return out, nil
}

// Load warm-loads a model into memory by sending a generate request with
// no prompt — Ollama's documented load mechanism. pin sets keep_alive=-1
// so the model is exempt from Ollama's idle TTL; unpinned loads use the
// daemon's default keep_alive.
func (o *Driver) Load(ctx context.Context, modelID string, pin bool) error {
	payload := map[string]any{"model": modelID}
	if pin {
		payload["keep_alive"] = -1
	}
	req, err := o.postJSON(ctx, "/api/generate", payload)
	if err != nil {
		return err
	}
	resp, err := o.do(req, "load")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Unload asks Ollama to drop the model from memory by sending a no-op
// generate request with keep_alive=0. This is Ollama's documented
// unload mechanism — it does not delete the weights from disk.
func (o *Driver) Unload(ctx context.Context, modelID string) error {
	req, err := o.postJSON(ctx, "/api/generate", map[string]any{
		"model":      modelID,
		"keep_alive": 0,
	})
	if err != nil {
		return err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return engines.Unreachable(name, o.endpoint, err)
	}
	defer resp.Body.Close()
	// 404 from /api/generate means Ollama doesn't recognize the model
	// name — for an unload caller, the desired post-state ("not in RAM")
	// already holds. Treat as success so `opod model unload` doesn't
	// red-error when the user calls it on a not-currently-loaded model
	// (the whole point of the command is to free RAM, which is already
	// the case).
	if resp.StatusCode == http.StatusNotFound {
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return engines.Upstream(name, "unload", resp.StatusCode, b)
	}
	return nil
}

// Delete removes a model from Ollama.
func (o *Driver) Delete(ctx context.Context, modelID string) error {
	body, _ := json.Marshal(map[string]any{"name": modelID})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, o.endpoint+"/api/delete", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.do(req, "delete")
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// Chat runs a chat completion. Events are emitted on the returned channel
// until Done or an error.
func (o *Driver) Chat(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	ctx, span := engines.StartChatSpan(ctx, name, req.Model, o.endpoint, len(req.Messages))
	// span.End() is deferred in the streaming goroutine so its duration
	// covers the whole streamed response. Synchronous errors close it
	// inline below.

	out := make(chan engines.StreamEvent, 16)
	httpReq, err := o.postJSON(ctx, "/api/chat", buildChatBody(req))
	if err != nil {
		span.MarkError("new request", err)
		span.End()
		close(out)
		return nil, err
	}

	resp, err := o.client.Do(httpReq)
	if err != nil {
		span.MarkError("http do", err)
		span.End()
		close(out)
		return nil, engines.Unreachable(name, o.endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		span.SetHTTPStatus(resp.StatusCode)
		span.MarkError(resp.Status, nil)
		span.End()
		close(out)
		return nil, engines.Upstream(name, "chat", resp.StatusCode, b)
	}
	span.SetHTTPStatus(resp.StatusCode)

	go func() {
		defer close(out)
		defer resp.Body.Close()
		defer span.End() // closes once the stream is drained or ctx cancels
		var promptTokens, completionTokens int
		defer func() { span.SetTokens(promptTokens, completionTokens) }()
		send := func(ev engines.StreamEvent) bool {
			select {
			case out <- ev:
				return true
			case <-ctx.Done():
				span.SetStatus(codes.Error, "client disconnected")
				return false
			}
		}
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for sc.Scan() {
			if ctx.Err() != nil {
				return
			}
			line := sc.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev struct {
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
				Done            bool   `json:"done"`
				DoneReason      string `json:"done_reason,omitempty"`
				PromptEvalCount int    `json:"prompt_eval_count,omitempty"`
				EvalCount       int    `json:"eval_count,omitempty"`
				Error           string `json:"error,omitempty"`
			}
			if err := json.Unmarshal(line, &ev); err != nil {
				send(engines.StreamEvent{Err: fmt.Errorf("decode: %w", err)})
				return
			}
			if ev.Error != "" {
				send(engines.StreamEvent{Err: fmt.Errorf("ollama: %s", ev.Error)})
				return
			}
			if ev.Done {
				// With Stream:false, Ollama returns a single JSON object
				// that has both done:true AND the full message content.
				// Emit the Delta first so callers running non-streaming
				// see the text in the stream, then close with Done+Usage.
				if ev.Message.Content != "" {
					if !send(engines.StreamEvent{Delta: ev.Message.Content}) {
						return
					}
				}
				promptTokens = ev.PromptEvalCount
				completionTokens = ev.EvalCount
				usage := &engines.Usage{
					PromptTokens:     ev.PromptEvalCount,
					CompletionTokens: ev.EvalCount,
					TotalTokens:      ev.PromptEvalCount + ev.EvalCount,
				}
				span.SetStatus(codes.Ok, "")
				send(engines.StreamEvent{Done: true, Usage: usage, Reason: reasonFrom(ev.DoneReason)})
				return
			}
			if ev.Message.Content != "" {
				if !send(engines.StreamEvent{Delta: ev.Message.Content}) {
					return
				}
			}
		}
		if err := sc.Err(); err != nil {
			send(engines.StreamEvent{Err: fmt.Errorf("stream: %w", err)})
		}
	}()

	return out, nil
}

func reasonFrom(s string) string {
	switch s {
	case "stop", "":
		return "stop"
	case "length":
		return "length"
	default:
		return s
	}
}

func buildChatBody(req engines.ChatRequest) map[string]any {
	// Ollama's chat schema:
	//   {"role": "...", "content": "...", "images": ["<base64 or url>", ...]}
	// We use map[string]any (not map[string]string) so we can include the
	// optional "images" field only when the caller actually attached one.
	msgs := make([]map[string]any, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		entry := map[string]any{"role": m.Role, "content": m.Content}
		if len(m.Images) > 0 {
			entry["images"] = m.Images
		}
		msgs = append(msgs, entry)
	}
	options := map[string]any{}
	if req.Temperature != nil {
		options["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		options["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		options["num_predict"] = *req.MaxTokens
	}
	if len(req.Stop) > 0 {
		options["stop"] = req.Stop
	}
	body := map[string]any{
		"model":    req.Model,
		"messages": msgs,
		"stream":   req.Stream,
	}
	if len(options) > 0 {
		body["options"] = options
	}
	return body
}

// ensure interface compliance at compile time
var (
	_ engines.Engine         = (*Driver)(nil)
	_ engines.EmbedEngine    = (*Driver)(nil)
	_ engines.ResidentLister = (*Driver)(nil)
	_ engines.Loader         = (*Driver)(nil)
)
