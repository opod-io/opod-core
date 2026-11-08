package leader

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/opod-io/opod-core/internal/api"
	"github.com/opod-io/opod-core/internal/router"
)

// dispatchOpenAIChat serves POST /v1/chat/completions: it reads the body's
// "model", maps "auto" to the leader's default model, and hands the request
// to routeOneOpenAI. Core proxies to no vendor (ADR-022): every request is
// served by this endpoint's own workers, gangs or engine.
func (s *Server) dispatchOpenAIChat(w http.ResponseWriter, r *http.Request) {
	// The request's arrival, before the body read and the admission gate: the
	// time to first token and the usage row's latency are measured from here,
	// so they include the wait for a slot (api/arrival.go).
	r = r.WithContext(api.WithArrival(r.Context(), time.Now()))
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeBodyReadError(w, err)
		return
	}
	model := peekModel(body)
	if isAutoModel(model) && s.cfg.Router.DefaultModel != "" {
		// "auto" → this leader's default model (ADR-022: the vendor routing chain left core)
		model = s.cfg.Router.DefaultModel
		body = setModel(body, model)
	}
	s.routeOneOpenAI(w, r, model, body)
}

// routeOneOpenAI serves one concrete model on the OpenAI path: the plan's
// one-model check, the admission gate, then the OpenAI handler. Called for
// /v1/chat/completions and by the Anthropic Messages and Vertex routes, which
// translate their body into a chat request first.
func (s *Server) routeOneOpenAI(w http.ResponseWriter, r *http.Request, model string, body []byte) {
	// One model identity per leader (D5): when a plan file is mounted, this
	// endpoint serves exactly that model — refuse anything else up front.
	if planModel, ok := s.planAllowsModel(model); !ok {
		writeJSONError(w, http.StatusNotFound,
			"this endpoint serves only "+planModel+" (requested "+model+")")
		return
	}
	// Admission (ADR-091, admission.go): the request takes a worker SLOT of
	// its model's pool, or waits for one in its class queue (ADR-086), or is
	// shed. Nothing able to serve at all is ADR-082's case and answers an
	// honest 503 + Retry-After rather than the 502/404 the local-engine
	// fallback would produce — asked PER MODEL (capacity.go), and the 503 is
	// the autoscaler's wake signal (/loadz unavailable_1m). The request's
	// class and flow are read on the way in, from memory.
	started := time.Now()
	// The route trace goes in before the gate, so the gate reads the worker
	// the router sends this request to (the handler reuses the same trace):
	// a request stranded on a worker that left the pool holds none of the
	// slots that remain (admission.go).
	r = r.WithContext(router.WithTrace(r.Context()))
	// The caller's cache salt (cachesalt.go): the router puts it on the
	// request and in its prefix pins.
	r = r.WithContext(router.WithCacheSalt(r.Context(), s.requestCacheSalt(r.Context())))
	// The conversation, and the worker an echoed route token names
	// (session.go): before the ident, which counts the session's turns.
	r = s.withSession(r, body, model)
	w = s.wrapRoute(w, r, model)
	ctx := withAdmissionIdent(r.Context(), s.requestIdent(r))
	if !s.capacityFor(ctx, model).servable() {
		// A sleeping engine is resumed for this request (wakeonrequest.go);
		// its resume signals capacityChanged, so the gate below sees it.
		s.wakeForRequest(ctx, model)
	}
	release, reason := s.admit(ctx, model, started)
	if reason != "" {
		// Policy fallback (P12-2): forward instead of 503 when the snapshot
		// names a target.
		if fb := s.policy.fallbackRouting(); fb != nil {
			if s.forwardToFallback(w, r, body, fb) {
				return
			}
		}
		w.Header().Set("Retry-After", "10")
		writeJSONError(w, http.StatusServiceUnavailable, reason)
		return
	}
	// The slot is the request's until its answer is written — for a stream,
	// its last byte: the handler returns only then. That return is the
	// completion that frees the slot for the next waiter.
	defer release()
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.openaiH.ChatCompletions(w, r)
	s.countSession(r.Context(), time.Now())
}

// writeBodyReadError maps a request-body read failure to the matching
// OpenAI-shaped error: 413 `request_too_large` when the route's cap tripped,
// 400 otherwise — the same writer the handlers use (api.BodyReadError), so
// the surface answers one way whichever reader hit the cap first (T15.3).
func writeBodyReadError(w http.ResponseWriter, err error) { api.BodyReadError(w, err) }

func peekModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}
