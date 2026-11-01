package leader

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

func gwServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil), st
}

// T11.1 / ADR-063 — usage is PUSHED to the leader at-least-once, so a
// gateway's retry after a half-written response must not bill twice. The
// dedup key is the id the GATEWAY minted, because only the gateway knows that
// two pushes are the same request.
func TestPushedUsageIsDeduplicatedByTheGatewaysRowID(t *testing.T) {
	srv, _ := gwServer(t)
	push := func(body string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.pushUsage(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/usage/push", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	rows := `{"gateway":"gw-a","rows":[
	  {"id":"r1","api_key_id":"k1","model":"m","prompt_tokens":10,"completion_tokens":5,"outcome":"ok"},
	  {"id":"r2","api_key_id":"k1","model":"m","prompt_tokens":20,"completion_tokens":5,"outcome":"ok"}]}`

	if got := push(rows); got["accepted"] != float64(2) || got["duplicate"] != float64(0) {
		t.Fatalf("first push takes both rows: %v", got)
	}
	// The same push again — a retry after a response the gateway never saw.
	if got := push(rows); got["accepted"] != float64(0) || got["duplicate"] != float64(2) {
		t.Fatalf("a retry must be recognised, not billed again: %v", got)
	}
	// 40 tokens, once: a retry must not have doubled the record.
	sum := 0
	for _, u := range srv.usage.Records() {
		sum += u.PromptTokens + u.CompletionTokens
	}
	if sum != 40 {
		t.Errorf("tokens recorded = %d, want 40 (a retry doubled the bill)", sum)
	}
	// A row with no id is the caller's own risk, and is taken rather than
	// silently dropped.
	if got := push(`{"gateway":"gw-a","rows":[{"api_key_id":"k1","prompt_tokens":1,"outcome":"ok"}]}`); got["accepted"] != float64(1) {
		t.Errorf("a row with no id is accepted (at-least-once is the caller's choice): %v", got)
	}
}

// T32.47: a door's row keeps its cached tokens, class and operation into the
// leader's queue, so a record served through a door says as much as one the
// leader served itself.
func TestADoorsRowKeepsItsCachedTokensClassAndOperation(t *testing.T) {
	srv, _ := gwServer(t)
	rec := httptest.NewRecorder()
	srv.pushUsage(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/usage/push", strings.NewReader(
		`{"gateway":"gw-a","rows":[{"id":"r1","model":"m","prompt_tokens":900,"cached_tokens":640,"class":"batch","operation":"chat","outcome":"ok"}]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("push: %d %s", rec.Code, rec.Body.String())
	}
	got := srv.usage.Records()
	if len(got) != 1 || got[0].CachedTokens != 640 || got[0].Class != "batch" || got[0].Operation != "chat" || got[0].Gateway != "gw-a" {
		t.Fatalf("records = %+v", got)
	}
}
