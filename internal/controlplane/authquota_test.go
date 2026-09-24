package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// TestAuthSnapshotCarriesTheDailyQuota: a managed key's daily ceiling arrives
// through the auth snapshot, follows an edit, and is ENFORCED.
//
// Why this test did not exist, and why the defect it covers was invisible:
// every other quota test — gatewayrole, gatewayspend — sets
// `APIKey.QuotaDailyTokens` on a LOCALLY minted record and then asserts the
// middleware refuses the request. That path was always correct. What nothing
// exercised is the only path a customer's key actually takes: the control
// plane mints it, writes it into the mounted auth snapshot, and the leader
// ingests the file. `applyAuthSnapshot` built the record without the field and
// had no update branch for it, so every managed key landed with quota 0,
// `QuotaMiddleware` read that as "unlimited", and the ceiling an operator set
// in the control plane did nothing at all. The middleware was not broken; the
// number never reached it.
//
// The field itself is opod-sdk adminapi's `SnapshotKey.QuotaDailyTokens`
// (v0.2.4). A leader older than that tag ignores it, as JSON does.
func TestAuthSnapshotCarriesTheDailyQuota(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = true
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)

	plain := "sk-orc-QUOTA-key"
	hash := auth.Hash(plain)
	apply := func(rev string, quota int64) {
		t.Helper()
		if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{
			Revision:    rev,
			RequireKeys: true,
			Keys:        []SnapshotKey{{ID: "q", Name: "team-q", Hash: hash, QuotaDailyTokens: quota}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	stored := func() int64 {
		t.Helper()
		k, err := st.APIKeys().GetByHash(ctx, hash)
		if err != nil || k == nil {
			t.Fatalf("key not in store: %v", err)
		}
		return k.QuotaDailyTokens
	}

	// 1. It arrives on CREATE. This is the line that was missing.
	apply("r1", 1000)
	if got := stored(); got != 1000 {
		t.Fatalf("quota on create: got %d, want 1000 — the snapshot's ceiling never reached the key", got)
	}

	// 2. It follows an EDIT, like the rate limits beside it. A control plane
	//    that raises a customer's ceiling must not need a new key id.
	apply("r2", 2500)
	if got := stored(); got != 2500 {
		t.Fatalf("quota after edit: got %d, want 2500", got)
	}

	// 3. It can be CLEARED. 0 is "no quota", not "leave what was there" —
	//    otherwise a ceiling could be raised for ever but never lifted.
	apply("r3", 0)
	if got := stored(); got != 0 {
		t.Fatalf("quota cleared: got %d, want 0", got)
	}

	// 4. And it BITES. Set a ceiling, spend past it, and the gateway refuses —
	//    the whole point of carrying the number.
	apply("r4", 100)
	keyRec, _ := st.APIKeys().GetByHash(ctx, hash)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	call := func() int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := call(); c != http.StatusOK {
		t.Fatalf("under quota: got %d, want 200", c)
	}
	if err := st.Usage().Record(ctx, store.Usage{
		TS: time.Now().UTC(), APIKeyID: keyRec.ID, Model: "m1", Protocol: "openai",
		PromptTokens: 90, CompletionTokens: 30, Outcome: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	if c := call(); c != http.StatusTooManyRequests {
		t.Fatalf("over quota (120 of 100 tokens spent): got %d, want 429", c)
	}
}
