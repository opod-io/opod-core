package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/router"
)

// T32.47: a record says what the request did (its route), how many prompt
// tokens the engine served from its cache, and the admission class that
// ranked it — and says nothing for a class when there was none.
func TestARecordNamesItsOperationCachedTokensAndClass(t *testing.T) {
	routes := []struct {
		op, body string
		route    func(h *Handler) http.HandlerFunc
	}{
		{OpChat, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`, func(h *Handler) http.HandlerFunc { return h.ChatCompletions }},
		{OpEmbeddings, `{"model":"m","input":"hi"}`, func(h *Handler) http.HandlerFunc { return h.Embeddings }},
		{OpRerank, `{"model":"m","query":"q","documents":["a","b"]}`, func(h *Handler) http.HandlerFunc { return h.Rerank }},
	}
	for _, rt := range routes {
		sink := newUsageSink()
		h := &Handler{Engine: &everythingEngine{finishingEngine{cancelEngine{deltas: []string{"Hel", "lo"}}}},
			Store: usageTestStore(t), Usage: sink.add, Default: "m"}
		rec := httptest.NewRecorder()
		rt.route(h)(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(rt.body)))
		if r := waitForUsage(t, sink); r.Operation != rt.op || r.Class != "" {
			t.Fatalf("%s: record %+v, want operation %s and no class", rt.op, r, rt.op)
		}
	}

	sink := newUsageSink()
	h := &Handler{Engine: &cancelEngine{}, Store: usageTestStore(t), Usage: sink.add}
	ctx := router.WithTrace(context.Background())
	router.NoteClass(ctx, "batch") // what the leader's gate does when the policy has classes
	h.recordUsage(ctx, OpChat, "m", &engines.Usage{PromptTokens: 900, CompletionTokens: 4, CachedTokens: 640}, time.Millisecond, "ok")
	if r := waitForUsage(t, sink); r.CachedTokens != 640 || r.Class != "batch" || r.Protocol != "openai" {
		t.Fatalf("record %+v, want 640 cached tokens, class batch, protocol openai", r)
	}
}
