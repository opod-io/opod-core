package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
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

// The rate limit is the part that is easy to get wrong: a flat share hands a
// keep-alive client, pinned to ONE door, 1/N of the rate it was sold. So the
// snapshot carries the ceiling AND this door's share, computed from the doors
// the leader has actually heard from.
func TestTheSpendSnapshotSharesTheRateAcrossTheDoorsItHasHeardFrom(t *testing.T) {
	srv, st := gwServer(t)
	ctx := context.Background()
	if err := st.APIKeys().Create(ctx, store.APIKey{ID: "k1", Hash: "h1", Name: "k1", Scope: "user",
		QuotaDailyTokens: 1000, RPMLimit: 60, TPMLimit: 3, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	snap := func() spendSnapshot {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.spend(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/spend", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("spend: %d %s", rec.Code, rec.Body.String())
		}
		var out spendSnapshot
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	// No gateway has pushed: the leader's own front door is the only one, so a
	// key keeps the whole rate it was sold. A share of 1/0 has no meaning.
	s0 := snap()
	if s0.Doors != 1 {
		t.Fatalf("with no gateway heard from, there is one door: %d", s0.Doors)
	}
	if s0.LagBoundMS != spendLagBoundMS {
		t.Errorf("the lag bound is PUBLISHED, not implied (ADR-063): %d", s0.LagBoundMS)
	}
	if len(s0.Keys) != 1 || s0.Keys[0].RPMShare != 60 {
		t.Fatalf("one door holds the whole ceiling: %+v", s0.Keys)
	}

	// Three doors push: each may use a third.
	now := time.Now()
	for _, gw := range []string{"gw-a", "gw-b", "gw-c"} {
		srv.gateways.note(gw, fmt.Sprintf("row-%s", gw), now)
	}
	s3 := snap()
	if s3.Doors != 3 || len(s3.Gateways) != 3 {
		t.Fatalf("three doors heard from: %d %v", s3.Doors, s3.Gateways)
	}
	if s3.Keys[0].RPMShare != 20 {
		t.Errorf("rpm share = %d, want 20 (60 across three doors)", s3.Keys[0].RPMShare)
	}
	// A ceiling too small to divide must not round to zero — a door that may
	// serve nothing is a door that is down.
	if s3.Keys[0].TPMShare != 1 {
		t.Errorf("tpm share = %d: a limit of 3 over 3 doors is 1, and a share must never be 0", s3.Keys[0].TPMShare)
	}
	// The ceilings themselves are reported unchanged, so a gateway can say what
	// the key was actually sold.
	if s3.Keys[0].RPMLimit != 60 || s3.Keys[0].QuotaDaily != 1000 {
		t.Errorf("the ceiling is reported beside the share: %+v", s3.Keys[0])
	}

	// A door that stops pushing stops counting, or every other door widens its
	// share and overshoots what the key was sold.
	srv.gateways.mu.Lock()
	srv.gateways.lastSeen["gw-c"] = now.Add(-2 * gatewayLiveFor)
	srv.gateways.mu.Unlock()
	if s := snap(); s.Doors != 2 || s.Keys[0].RPMShare != 30 {
		t.Errorf("a silent door drops out: doors=%d share=%d", s.Doors, s.Keys[0].RPMShare)
	}

	// An unlimited key stays unlimited: a share of "no ceiling" is no ceiling.
	if err := st.APIKeys().Create(ctx, store.APIKey{ID: "k2", Hash: "h2", Name: "k2", Scope: "user", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, k := range snap().Keys {
		if k.APIKeyID == "k2" && (k.RPMShare != 0 || k.TPMShare != 0) {
			t.Errorf("an unlimited key must not acquire a share: %+v", k)
		}
	}
}
