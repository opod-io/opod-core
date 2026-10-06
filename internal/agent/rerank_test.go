package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/sglang" // a worker's own engine in the hop test
	_ "github.com/opod-io/opod/internal/engines/vllm"   // how a leader dials a worker
)

// The whole hop (ADR-084): a leader dials a worker through the vLLM driver, the
// worker answers from its own engine — here SGLang, whose answer is a bare
// array — and the leader reads one score per document either way.
func TestRerankCrossesTheLeaderToWorkerHop(t *testing.T) {
	engineSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			t.Errorf("engine got %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"score":0.8,"index":1},{"score":0.3,"index":0}]`))
	}))
	defer engineSrv.Close()

	worker := &Server{Engine: engines.MustNew("sglang", engineSrv.URL, ""), Token: "tok"}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/rerank", worker.auth(worker.rerank))
	workerSrv := httptest.NewServer(mux)
	defer workerSrv.Close()

	leaderSide := engines.MustNew("vllm", workerSrv.URL, "tok").(engines.RerankEngine)
	got, err := leaderSide.Rerank(context.Background(), engines.RerankRequest{Model: "m", Query: "q", Documents: []string{"a", "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Results) != 2 || got.Results[0] != (engines.RerankResult{Index: 1, Score: 0.8}) {
		t.Fatalf("results did not survive the hop: %+v", got.Results)
	}
}

// A worker whose engine has no rerank route answers a NAMED 501 the leader
// reads as ErrRerankNotSupported — never a 404, never a 502.
func TestWorkerWithoutRerankSaysSoByName(t *testing.T) {
	s := &Server{Engine: &chatRecorder{}}
	w := httptest.NewRecorder()
	s.rerank(w, httptest.NewRequest(http.MethodPost, "/v1/rerank",
		strings.NewReader(`{"model":"m","query":"q","documents":["a"]}`)))
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501: %s", w.Code, w.Body.String())
	}
	var env struct {
		Error struct{ Type string } `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil || env.Error.Type != "rerank_not_supported" {
		t.Fatalf("not a named refusal: %s", w.Body.String())
	}

	// And the leader's side of the same answer.
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		s.rerank(rw, r)
	}))
	defer srv.Close()
	_, err := engines.MustNew("vllm", srv.URL, "").(engines.RerankEngine).Rerank(context.Background(),
		engines.RerankRequest{Model: "m", Query: "q", Documents: []string{"a"}})
	if !errors.Is(err, engines.ErrRerankNotSupported) {
		t.Fatalf("leader read %v, want ErrRerankNotSupported", err)
	}
}
