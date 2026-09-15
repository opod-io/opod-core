package api

// Embeddings go through the same attribution and routing rules as chat: the
// engine receives the CATALOG id (the router resolves the native name at
// dispatch; the native name here made placement lookups miss every worker)
// and the usage row names the worker that answered (audit 2026-09-07, C3 #4
// and #5 — embeddings were the one protocol whose rows carried no NodeID).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	catalog "github.com/opod-io/opod-sdk/catalog"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/router"
	"github.com/opod-io/opod/internal/store"
)

// embedStub stands in for the router: it records the model name it was
// handed and notes a worker on the request's trace, as a dispatch does.
type embedStub struct {
	engines.Engine
	gotModel string
}

func (e *embedStub) Name() string { return "vllm" }
func (e *embedStub) Embed(ctx context.Context, req engines.EmbedRequest) (engines.EmbedResponse, error) {
	e.gotModel = req.Model
	router.NoteNode(ctx, "w_1")
	return engines.EmbedResponse{Vectors: [][]float32{{0.1, 0.2}}, Usage: &engines.Usage{PromptTokens: 2, TotalTokens: 2}}, nil
}

func TestEmbeddings_RoutesByCatalogIDAndAttributesTheWorker(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stub := &embedStub{}
	h := &Handler{Engine: stub, Store: st,
		// A catalog entry whose engine-native name differs from its id, so a
		// handler that resolved too early would hand the engine "org/fixture-embed".
		Catalog: []catalog.Entry{{ID: "fixture-embed", Source: catalog.Source{Repo: "org/fixture-embed"}}}}

	req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"fixture-embed","input":"hello"}`))
	rec := httptest.NewRecorder()
	h.Embeddings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out embeddingResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Data) != 1 {
		t.Fatalf("response: %v %s", err, rec.Body.String())
	}
	if stub.gotModel != "fixture-embed" {
		t.Fatalf("engine got %q, want the catalog id (the router resolves the native name at dispatch)", stub.gotModel)
	}
	rows, err := st.Usage().Recent(context.Background(), 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("usage rows: %v %v", rows, err)
	}
	if rows[0].NodeID != "w_1" || rows[0].Model != "fixture-embed" || rows[0].Outcome != "ok" {
		t.Fatalf("usage row %+v: want NodeID w_1, model fixture-embed, outcome ok", rows[0])
	}
}
