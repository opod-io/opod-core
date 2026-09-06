package controlplane

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/opod-io/opod/internal/api"
)

func (s *Server) dispatchOpenAIChat(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		writeBodyReadError(w, err)
		return
	}
	model := peekModel(body)
	if isAutoModel(model) {
		// "auto" → walk the routing chain (rate-limit / failure failover).
		s.serveAutoOpenAI(w, r, body)
		return
	}
	s.routeOneOpenAI(w, r, model, body)
}

// routeOneOpenAI serves a single concrete model on the OpenAI path: a vendor
// model goes to its egress proxy, a local model to the local handler. Called
// both directly (specific model requested) and per-candidate by the auto
// chain walker (with w wrapped in a failoverWriter).
func (s *Server) routeOneOpenAI(w http.ResponseWriter, r *http.Request, model string, body []byte) {
	if vendor := api.Vendor(model); vendor != "" && s.cfg.Router.Fallback.Enabled {
		r.Body = io.NopCloser(bytes.NewReader(body))
		switch vendor {
		case "openai":
			s.egressH.ServeOpenAI(w, r)
		case "vertex":
			s.egressH.ServeVertex(w, r)
		case "openrouter":
			s.egressH.ServeOpenRouter(w, r)
		case "groq":
			s.egressH.ServeGroq(w, r)
		case "together":
			s.egressH.ServeTogether(w, r)
		case "fireworks":
			s.egressH.ServeFireworks(w, r)
		case "cohere":
			s.egressH.ServeCohere(w, r)
		case "mistral":
			s.egressH.ServeMistral(w, r)
		case "perplexity":
			s.egressH.ServePerplexity(w, r)
		case "anthropic", "bedrock":
			// Protocol mismatch: OpenAI-format request with a Claude model.
			// Anthropic's API only accepts /v1/messages, so return an actionable
			// error rather than forwarding garbage upstream.
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("model %q uses the Anthropic message shape; POST to /v1/messages instead of /v1/chat/completions", model))
		default:
			// Registry providers (deepseek/, cerebras/, gemini/, …) all speak
			// the OpenAI shape and route through the one generic handler.
			s.egressH.ServeGeneric(w, r, vendor)
		}
		return
	}
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
	if isAutoModel(model) {
		s.serveAutoAnthropic(w, r, body)
		return
	}
	s.routeOneAnthropic(w, r, model, body)
}

// routeOneAnthropic serves a single concrete model on the Anthropic path.
func (s *Server) routeOneAnthropic(w http.ResponseWriter, r *http.Request, model string, body []byte) {
	if vendor := api.Vendor(model); vendor != "" && s.cfg.Router.Fallback.Enabled {
		r.Body = io.NopCloser(bytes.NewReader(body))
		switch vendor {
		case "anthropic":
			s.egressH.ServeAnthropic(w, r)
		case "bedrock":
			s.egressH.ServeBedrock(w, r)
		default:
			// Protocol mismatch: Anthropic-format request with a non-Anthropic
			// (OpenAI-shape) model.
			writeJSONError(w, http.StatusBadRequest,
				fmt.Sprintf("model %q does not use the Anthropic message shape; POST to /v1/chat/completions instead", model))
		}
		return
	}
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
