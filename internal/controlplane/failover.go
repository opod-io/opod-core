package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/opod-io/opod/internal/api"
)

// isAutoModel reports whether a request asked for the special "auto" model,
// which means "walk the routing chain" rather than a specific model.
func isAutoModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), "auto")
}

// retryableStatus reports whether an attempt's status should advance the
// routing chain to the next candidate (vs. committing it to the client).
// Rate limits, transient 5xx, and not-found/timeout all mean "try the next
// provider"; a 400/401/403 is the caller's problem and is surfaced as-is.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusNotFound, // 404 — model not available here, try next
		http.StatusRequestTimeout,      // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// failoverWriter wraps the real ResponseWriter for one chain attempt. It
// buffers the handler's response *decision* until it can tell whether to
// commit it to the client or discard it and try the next candidate:
//
//   - WriteHeader(retryable) while another candidate remains → swallow, set
//     retry; the walker moves on and nothing reached the client.
//   - WriteHeader(non-retryable) or the last candidate → commit and become a
//     transparent pass-through (status + headers + streamed body).
//
// Because the egress/local handlers always write their status before any
// body, a single attempt is fully recoverable until that first write.
type failoverWriter struct {
	w         http.ResponseWriter
	canRetry  bool // another candidate remains after this one
	hdr       http.Header
	committed bool // decided to pass through to the client
	retry     bool // handler returned a retryable status; advance the chain
}

func newFailoverWriter(w http.ResponseWriter, canRetry bool) *failoverWriter {
	return &failoverWriter{w: w, canRetry: canRetry, hdr: http.Header{}}
}

func (f *failoverWriter) Header() http.Header {
	if f.committed {
		return f.w.Header()
	}
	return f.hdr
}

func (f *failoverWriter) WriteHeader(code int) {
	if f.committed || f.retry {
		return
	}
	if f.canRetry && retryableStatus(code) {
		f.retry = true
		return
	}
	f.commit(code)
}

func (f *failoverWriter) commit(code int) {
	dst := f.w.Header()
	for k, vs := range f.hdr {
		dst[k] = vs
	}
	f.w.WriteHeader(code)
	f.committed = true
}

func (f *failoverWriter) Write(b []byte) (int, error) {
	if f.retry {
		// Discard the failed attempt's body; the walker will try the next.
		return len(b), nil
	}
	if !f.committed {
		f.commit(http.StatusOK)
	}
	return f.w.Write(b)
}

func (f *failoverWriter) Flush() {
	if f.committed {
		if fl, ok := f.w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

// Unwrap lets http.ResponseController reach the underlying writer for controls
// we don't override (Hijack, deadlines). Flush stays on this type so the
// commit gate is still honored.
func (f *failoverWriter) Unwrap() http.ResponseWriter { return f.w }

// setModel rewrites the JSON body's "model" field to id (used to retarget a
// request at each chain candidate). Returns the body unchanged if it isn't
// valid JSON.
func setModel(body []byte, id string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = id
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// resolveChain returns the stored routing chain, or the computed default when
// none has been saved.
func (s *Server) resolveChain(ctx context.Context) []string {
	if chain, err := s.store.Route().Get(ctx); err == nil && len(chain) > 0 {
		return chain
	}
	return s.defaultRoute()
}

// openAICompatible reports whether a chain entry can be served on the OpenAI
// (/v1/chat/completions) path. Anthropic-shape and stubbed vendors can't, so
// the OpenAI auto-walk skips them (cross-protocol translation is future work).
func openAICompatible(model string) bool {
	switch api.Vendor(model) {
	case "anthropic", "bedrock", "vertex":
		return false
	}
	return true
}

// anthropicCompatible reports whether a chain entry can be served on the
// Anthropic (/v1/messages) path: local models or Anthropic-shape vendors only.
func anthropicCompatible(model string) bool {
	switch api.Vendor(model) {
	case "", "anthropic", "bedrock":
		return true
	}
	return false
}

func filterChain(chain []string, ok func(string) bool) []string {
	out := make([]string, 0, len(chain))
	for _, m := range chain {
		if ok(m) {
			out = append(out, m)
		}
	}
	return out
}

// serveAutoOpenAI walks the routing chain for a model="auto" request on the
// OpenAI path: try each compatible candidate in order, advancing on a
// rate-limit / transient failure, committing the first success (or the final
// candidate's response).
func (s *Server) serveAutoOpenAI(w http.ResponseWriter, r *http.Request, body []byte) {
	chain := filterChain(s.resolveChain(r.Context()), openAICompatible)
	if len(chain) == 0 {
		// Nothing servable — preserve legacy behavior (local handler resolves
		// "auto" itself).
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.openaiH.ChatCompletions(w, r)
		return
	}
	for i, cand := range chain {
		fw := newFailoverWriter(w, i < len(chain)-1)
		s.routeOneOpenAI(fw, r, cand, setModel(body, cand))
		if !fw.retry {
			return
		}
	}
}

// serveAutoAnthropic is the /v1/messages counterpart of serveAutoOpenAI.
func (s *Server) serveAutoAnthropic(w http.ResponseWriter, r *http.Request, body []byte) {
	chain := filterChain(s.resolveChain(r.Context()), anthropicCompatible)
	if len(chain) == 0 {
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.anthropicH.Messages(w, r)
		return
	}
	for i, cand := range chain {
		fw := newFailoverWriter(w, i < len(chain)-1)
		s.routeOneAnthropic(fw, r, cand, setModel(body, cand))
		if !fw.retry {
			return
		}
	}
}
