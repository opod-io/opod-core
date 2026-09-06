package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

func (s *Server) dispatchOpenAIChat(w http.ResponseWriter, r *http.Request) {
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
	// Router-only leader with nothing awake to serve: answer an honest
	// 503 "waking" instead of the 502 the dead local-engine fallback
	// would produce. The 503 itself is the autoscaler's wake signal
	// (it shows up in /loadz unavailable_1m).
	if !s.cfg.Router.PullDefaultModel && !s.hasServingCapacity(r.Context()) {
		w.Header().Set("Retry-After", "10")
		writeJSONError(w, http.StatusServiceUnavailable,
			"no workers are awake for this model — waking (scale-up in progress or floor is 0); retry shortly")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.openaiH.ChatCompletions(w, r)
}

func (s *Server) dispatchAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeBodyReadError(w, err)
		return
	}
	model := peekModel(body)
	if isAutoModel(model) && s.cfg.Router.DefaultModel != "" {
		model = s.cfg.Router.DefaultModel
		body = setModel(body, model)
	}
	s.routeOneAnthropic(w, r, model, body)
}

// routeOneAnthropic serves a single concrete model on the Anthropic path.
func (s *Server) routeOneAnthropic(w http.ResponseWriter, r *http.Request, model string, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.anthropicH.Messages(w, r)
}

// writeBodyReadError maps a request-body read failure to the matching
// JSON error: 413 when the limitRequestBody cap tripped, 400 otherwise.
func writeBodyReadError(w http.ResponseWriter, err error) {
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		writeJSONError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds the %d-byte limit", mbe.Limit))
		return
	}
	writeJSONError(w, http.StatusBadRequest, "read body: "+err.Error())
}

func peekModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}
