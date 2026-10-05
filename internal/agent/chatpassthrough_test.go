package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

type chatRecorder struct {
	engines.Engine
	got engines.ChatRequest
}

func (c *chatRecorder) Name() string { return "recorder" }
func (c *chatRecorder) Chat(_ context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	c.got = req
	ch := make(chan engines.StreamEvent, 2)
	ch <- engines.StreamEvent{Delta: "ok"}
	ch <- engines.StreamEvent{Done: true, Reason: "stop"}
	close(ch)
	return ch, nil
}

// The leader re-serialises a chat through the shared OpenAI body builder
// before it reaches a worker, so any field the builder sends and the worker's
// own request struct does not name is dropped HERE — one hop short of the
// engine and invisibly. That is the whole failure mode T10.14 exists to fix,
// and a worker is the normal path under the control plane, so the leader-local
// test alone would have proved nothing.
func TestWorkerCarriesResponseFormatToTheEngine(t *testing.T) {
	eng := &chatRecorder{}
	s := &Server{Engine: eng}

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}}`
	w := httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if len(eng.got.ResponseFormat) == 0 {
		t.Fatal("response_format never reached the engine: the worker dropped it")
	}
	var rf struct {
		Type       string `json:"type"`
		JSONSchema *struct {
			Name string `json:"name"`
		} `json:"json_schema"`
	}
	if err := json.Unmarshal(eng.got.ResponseFormat, &rf); err != nil {
		t.Fatalf("response_format is not the caller's JSON: %v (%q)", err, eng.got.ResponseFormat)
	}
	if rf.Type != "json_schema" || rf.JSONSchema == nil || rf.JSONSchema.Name != "r" {
		t.Fatalf("response_format changed in flight: %q", eng.got.ResponseFormat)
	}

	// A caller who asked for nothing must not have an empty object invented
	// for them on the way down.
	eng.got = engines.ChatRequest{}
	w = httptest.NewRecorder()
	s.chatCompletions(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
	if len(eng.got.ResponseFormat) != 0 {
		t.Fatalf("response_format was invented: %q", eng.got.ResponseFormat)
	}
}
