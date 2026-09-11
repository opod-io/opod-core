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

type embedEngine struct {
	engines.Engine
	got engines.EmbedRequest
}

func (e *embedEngine) Name() string { return "fake" }
func (e *embedEngine) Embed(_ context.Context, req engines.EmbedRequest) (engines.EmbedResponse, error) {
	e.got = req
	out := make([][]float32, len(req.Inputs))
	for i := range req.Inputs {
		out[i] = []float32{float32(i), 0.5}
	}
	return engines.EmbedResponse{Vectors: out, Usage: &engines.Usage{PromptTokens: 7}}, nil
}

type chatOnlyEngine struct{ engines.Engine }

func (chatOnlyEngine) Name() string { return "chat-only" }

// A worker answers only the routes it exposes. Embeddings were implemented once
// on the shared OpenAI-compatible driver (ADR-025) and worked on a local engine,
// but the worker server exposed chat alone — so the moment a leader routed an
// embeddings request to a worker it got a 404 whose message pointed at the
// wrong thing ("vllm POST /v1/embeddings: 404 page not found").
func TestWorkerServesEmbeddings(t *testing.T) {
	eng := &embedEngine{}
	s := &Server{Engine: eng}
	call := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		s.embeddings(w, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body)))
		return w
	}

	// A single string input, the shape the control plane's serving proof sends.
	w := call(`{"model":"m","input":"hi"}`)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var got struct {
		Object string `json:"object"`
		Data   []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
		Usage struct {
			PromptTokens int `json:"prompt_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("not the OpenAI shape: %v — %s", err, w.Body.String())
	}
	if got.Object != "list" || len(got.Data) != 1 || len(got.Data[0].Embedding) != 2 || got.Usage.PromptTokens != 7 {
		t.Fatalf("response = %s", w.Body.String())
	}
	if len(eng.got.Inputs) != 1 || eng.got.Inputs[0] != "hi" || eng.got.Model != "m" {
		t.Errorf("engine saw %+v", eng.got)
	}

	// An array of inputs comes back in the caller's order (ADR-025).
	w = call(`{"model":"m","input":["a","b","c"]}`)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got.Data) != 3 {
		t.Fatalf("array input: %s", w.Body.String())
	}
	for i, d := range got.Data {
		if d.Index != i || d.Embedding[0] != float64(i) {
			t.Errorf("vector %d out of order: %+v", i, d)
		}
	}

	// Bad input is a 400 that says what is wrong, not a 500.
	for _, bad := range []string{`{"model":"m"}`, `{"model":"m","input":""}`, `{"model":"m","input":[]}`, `{"model":"m","input":{"a":1}}`} {
		if w := call(bad); w.Code != 400 {
			t.Errorf("%s → %d, want 400", bad, w.Code)
		}
	}

	// An engine with no embeddings says so as 501 — the honest answer, and the
	// one the leader can report without guessing.
	w = httptest.NewRecorder()
	(&Server{Engine: chatOnlyEngine{}}).embeddings(w, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"m","input":"hi"}`)))
	if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "chat-only") {
		t.Errorf("engine without embeddings → %d %s", w.Code, w.Body.String())
	}
}
