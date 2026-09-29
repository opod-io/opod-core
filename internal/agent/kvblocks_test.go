package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod-sdk/nodeapi"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/kvevents"
)

type tokenizingEngine struct {
	engines.Engine
	gotModel string
	gotMsgs  int
}

func (e *tokenizingEngine) Name() string { return "vllm" }
func (e *tokenizingEngine) Tokenize(_ context.Context, model string, msgs []engines.Message, _ string, limit int) ([]int, error) {
	e.gotModel, e.gotMsgs = model, len(msgs)
	out := []int{1, 2, 3, 4, 5}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// The tokenize route answers from the engine's own tokenizer, bounded by the
// caller's max, and says 501 for an engine that has none — the leader then
// keeps its sticky pin.
func TestTokenizeRoute(t *testing.T) {
	call := func(s *Server, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		s.tokenize(rec, httptest.NewRequest(http.MethodPost, nodeapi.PathTokenize, bytes.NewReader(raw)))
		return rec
	}
	eng := &tokenizingEngine{}
	s := &Server{Engine: eng, Aliases: nil}
	rec := call(s, nodeapi.TokenizeRequest{Model: "m", MaxTokens: 3, Messages: []nodeapi.TokenizeMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}}})
	var out nodeapi.TokenizeResponse
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Tokens) != 3 {
		t.Fatalf("tokenize: %d %s", rec.Code, rec.Body.String())
	}
	if eng.gotModel != "m" || eng.gotMsgs != 2 {
		t.Fatalf("the engine was asked for %q with %d messages", eng.gotModel, eng.gotMsgs)
	}
	if rec := call(&Server{Engine: plainEngine{}, Aliases: nil}, nodeapi.TokenizeRequest{Model: "m"}); rec.Code != http.StatusNotImplemented {
		t.Fatalf("an engine with no tokenizer must answer 501, got %d", rec.Code)
	}
}

// The engine is told to publish only when the feature is on, and on localhost.
func TestKVEventsFlagIsLocalAndGated(t *testing.T) {
	if got := (&Server{}).kvEventsArgs(); got != "" {
		t.Fatalf("off by default: %q", got)
	}
	got := (&Server{KVEvents: true, Blocks: kvevents.NewTranslator()}).kvEventsArgs()
	if !strings.Contains(got, "--kv-events-config") || !strings.Contains(got, "tcp://127.0.0.1:") || strings.Contains(got, "tcp://*") {
		t.Fatalf("the publisher must bind localhost: %q", got)
	}
}
