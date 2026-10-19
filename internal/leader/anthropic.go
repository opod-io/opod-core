package leader

// Anthropic Messages on the leader (ADR-092 §1, PLAN T19.3): the request is
// translated to a chat request (api/anthropic.go) and served by the same path
// as /v1/chat/completions — the plan's one model, admission, the router, the
// guardrails, the usage row (protocol "anthropic") — and the answer is
// translated back on the way out.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/api"
)

// dispatchAnthropicMessages serves POST /v1/messages.
func (s *Server) dispatchAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(api.WithArrival(r.Context(), time.Now()))
	chat, model, ok := s.anthropicChat(w, r, false)
	if !ok {
		return
	}
	aw := api.NewAnthropicWriter(w, model)
	defer aw.Finish()
	r = api.WithRequestBody(r.WithContext(api.WithProtocol(r.Context(), api.ProtocolAnthropic)), chat)
	s.routeOneOpenAI(aw, r, model, chat)
}

// dispatchAnthropicCount serves POST /v1/messages/count_tokens. The leader has
// no tokenizer, and only some engines lend theirs, so the count is the
// engine's own: the prompt is served as a one-token generation and the answer
// is its prompt_tokens — exact on every engine, with the chat template and the
// tools in it. It is real work, so it takes a slot and is a usage row like any
// other request.
func (s *Server) dispatchAnthropicCount(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(api.WithArrival(r.Context(), time.Now()))
	chat, model, ok := s.anthropicChat(w, r, true)
	if !ok {
		return
	}
	rec := &bufferedWriter{header: http.Header{}, status: http.StatusOK}
	r = api.WithRequestBody(r.WithContext(api.WithProtocol(r.Context(), api.ProtocolAnthropic)), chat)
	s.routeOneOpenAI(rec, r, model, chat)
	for k, v := range rec.header {
		if k != "Content-Length" && k != "Content-Type" {
			w.Header()[k] = v
		}
	}
	if rec.status >= 300 {
		aw := api.NewAnthropicWriter(w, model)
		aw.WriteHeader(rec.status)
		_, _ = aw.Write(rec.body.Bytes())
		aw.Finish()
		return
	}
	var out struct {
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.body.Bytes(), &out); err != nil {
		api.WriteAnthropicError(w, &api.AnthropicError{Status: http.StatusBadGateway, Type: "api_error", Message: "the engine's answer could not be read: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"input_tokens": out.Usage.PromptTokens})
}

// anthropicChat reads a Messages body and translates it, answering the
// refusal itself when it cannot. "auto" (or no model) is this leader's default
// model, as it is on the chat route.
func (s *Server) anthropicChat(w http.ResponseWriter, r *http.Request, count bool) ([]byte, string, bool) {
	body, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		aw := api.NewAnthropicWriter(w, "")
		writeBodyReadError(aw, err)
		aw.Finish()
		return nil, "", false
	}
	chat, model, _, aerr := api.AnthropicToChat(body, count)
	if aerr != nil {
		api.WriteAnthropicError(w, aerr)
		return nil, "", false
	}
	if (model == "" || isAutoModel(model)) && s.cfg.Router.DefaultModel != "" {
		model = s.cfg.Router.DefaultModel
		chat = setModel(chat, model)
	}
	return chat, model, true
}

// bufferedWriter holds an answer for a caller that rewrites it whole.
type bufferedWriter struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) WriteHeader(code int) {
	if !b.wrote {
		b.status, b.wrote = code, true
	}
}

func (b *bufferedWriter) Write(p []byte) (int, error) {
	b.wrote = true
	return b.body.Write(p)
}
