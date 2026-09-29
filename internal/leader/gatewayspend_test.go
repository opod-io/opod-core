package leader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	srv, st := gwServer(t)
	ctx := context.Background()
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
	// 40 tokens, once: the quota the gateway subtracts from must not have
	// doubled.
	sum, err := st.Usage().SumTokensSince(ctx, "k1", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
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
