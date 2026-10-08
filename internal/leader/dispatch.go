package leader

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/api"
)

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

// routeOneOpenAI serves a single concrete model on the OpenAI path: a vendor
// model goes to its egress proxy, a local model to the local handler. Called
// both directly (specific model requested) and per-candidate by the auto
// chain walker (with w wrapped in a failoverWriter).
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
	ctx := withAdmissionIdent(r.Context(), s.requestIdent(r))
	if !s.capacityFor(ctx, model).servable() {
		// A sleeping engine is resumed for this request (wakeonrequest.go);
		// its resume signals capacityChanged, so the gate below sees it.
		s.wakeForRequest(ctx, model)
	}
	release, reason := s.admit(ctx, model, started)
	if reason != "" {
		// Policy fallback (P12-2): forward instead of 503 when the snapshot
		// names a target — after the pre guardrail chain, so a blocked prompt
		// is refused here and never leaves the endpoint.
		if fb := s.policy.fallbackRouting(); fb != nil {
			checked, ok := api.ApplyPreCallGuardrails(r.Context(), w, s.store, s.openaiH.Policy().Guardrails, body)
			if !ok {
				return
			}
			if s.forwardToFallback(w, r, checked, fb) {
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
