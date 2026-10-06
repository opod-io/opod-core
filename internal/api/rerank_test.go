package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// rerankEngine scores documents with fixed numbers, in no particular order.
type rerankEngine struct {
	cancelEngine
	scores []float64
	err    error
}

func (e *rerankEngine) Rerank(_ context.Context, req engines.RerankRequest) (engines.RerankResponse, error) {
	if e.err != nil {
		return engines.RerankResponse{}, e.err
	}
	out := engines.RerankResponse{Usage: &engines.Usage{PromptTokens: 9, TotalTokens: 9}}
	for i := len(req.Documents) - 1; i >= 0; i-- {
		out.Results = append(out.Results, engines.RerankResult{Index: i, Score: e.scores[i]})
	}
	return out, nil
}

func postRerank(t *testing.T, h *Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Rerank(rec, httptest.NewRequest(http.MethodPost, "/v1/rerank", strings.NewReader(body)))
	return rec
}

type rerankAnswer struct {
	Model   string `json:"model"`
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float64 `json:"relevance_score"`
		Document       *struct {
			Text string `json:"text"`
		} `json:"document"`
	} `json:"results"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
	} `json:"usage"`
}

// The answer is the same whatever the engine did: most relevant first,
// truncated to top_n, the text attached from the request (ADR-084).
func TestRerankOrdersTruncatesAndAttachesText(t *testing.T) {
	st := usageTestStore(t)
	h := &Handler{Engine: &rerankEngine{scores: []float64{0.2, 0.9, 0.5}}, Store: st}
	rec := postRerank(t, h, `{"model":"rr","query":"q","documents":["a","b","c"],"top_n":2}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got rerankAnswer
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 || got.Results[0].Index != 1 || got.Results[1].Index != 2 {
		t.Fatalf("not ordered and truncated: %s", rec.Body.String())
	}
	if got.Results[0].Document == nil || got.Results[0].Document.Text != "b" || got.Usage.PromptTokens != 9 {
		t.Fatalf("text or usage missing: %s", rec.Body.String())
	}
	// Recorded like embeddings: one usage row with the engine's tokens.
	if u := waitForUsage(t, st); u.Model != "rr" || u.Outcome != "ok" || u.PromptTokens != 9 {
		t.Fatalf("usage row: %+v", u)
	}

	// return_documents:false leaves the text out; no top_n returns all.
	rec = postRerank(t, h, `{"model":"rr","query":"q","documents":["a","b","c"],"return_documents":false}`)
	got = rerankAnswer{}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if len(got.Results) != 3 || got.Results[0].Document != nil {
		t.Fatalf("return_documents:false or the full list: %s", rec.Body.String())
	}
}

// An engine with no rerank route is refused by NAME, as 501 — never a 404 that
// reads like a wrong URL.
func TestRerankOnAnEngineThatCannotIsANamed501(t *testing.T) {
	for name, eng := range map[string]engines.Engine{
		"no interface":   &cancelEngine{},
		"engine says no": &rerankEngine{err: fmt.Errorf("worker w1: %w", engines.ErrRerankNotSupported)},
	} {
		t.Run(name, func(t *testing.T) {
			h := &Handler{Engine: eng, Store: usageTestStore(t)}
			rec := postRerank(t, h, `{"model":"rr","query":"q","documents":["a"]}`)
			if rec.Code != http.StatusNotImplemented || !strings.Contains(rec.Body.String(), `"rerank_not_supported"`) {
				t.Fatalf("got %d %s, want 501 rerank_not_supported", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestRerankRefusesAMalformedRequest(t *testing.T) {
	h := &Handler{Engine: &rerankEngine{scores: []float64{1}}, Store: usageTestStore(t)}
	for _, body := range []string{
		`{}`,
		`{"model":"rr","documents":["a"]}`,
		`{"model":"rr","query":"q","documents":[]}`,
		`{"model":"rr","query":"q","documents":["a"],"top_n":-1}`,
		`{"query":"q","documents":["a"]}`,
		`{"model":"rr","query":"q","documents":[{"text":"a"}]}`,
	} {
		if rec := postRerank(t, h, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s → %d, want 400: %s", body, rec.Code, rec.Body.String())
		}
	}
}
