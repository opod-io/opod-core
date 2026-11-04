package leader

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// TestAuthSnapshotSync: keys from the snapshot land in the store as hashes,
// limits follow the file, a key that leaves the file is revoked, locally
// minted keys are untouched, and requireKeys flips the gateway at runtime.
func TestAuthSnapshotSync(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = false // managed leaders start keyless; the snapshot decides
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)

	// a locally minted key must survive every sync
	localPlain, localRec, _ := auth.Generate("local", "admin", "")
	if err := st.APIKeys().Create(ctx, localRec); err != nil {
		t.Fatal(err)
	}

	plainA, plainB := "sk-orc-AAAA", "sk-orc-BBBB"
	snap := &AuthSnapshot{Revision: "r1", RequireKeys: true, Keys: []SnapshotKey{
		{ID: "a", Name: "team-a", Hash: auth.Hash(plainA)},
		{ID: "b", Name: "team-b", Hash: auth.Hash(plainB)},
	}}
	if err := srv.applyAuthSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if !srv.requireKeys() {
		t.Fatal("requireKeys must follow the snapshot")
	}
	ka, _ := st.APIKeys().GetByHash(ctx, auth.Hash(plainA))
	if ka == nil || ka.ID != SnapshotKeyPrefix+"a" || ka.Scope != "user" {
		t.Fatalf("key a not synced: %+v", ka)
	}

	// gateway: the snapshot key passes, garbage is refused, local admin key still works
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	code := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := code(""); c != http.StatusUnauthorized {
		t.Fatalf("no key with requireKeys=true: %d", c)
	}
	if c := code(plainA); c != http.StatusOK {
		t.Fatalf("snapshot key: %d", c)
	}
	if c := code(localPlain); c != http.StatusOK {
		t.Fatalf("local admin key: %d", c)
	}

	// second snapshot: key b leaves, requireKeys off. It still names the per-key
	// limits a manager wrote before ADR-112: the leader enforces none of them —
	// limits are the owner's gateway's job — and the key stays as it was.
	snap2 := &AuthSnapshot{}
	if err := json.Unmarshal([]byte(`{"revision":"r2","requireKeys":false,"keys":[{"id":"a","name":"team-a","hash":"`+auth.Hash(plainA)+`","rpmLimit":20,"tpmLimit":5,"quotaDailyTokens":1000,"allowedModels":["x"]}]}`), snap2); err != nil {
		t.Fatal(err)
	}
	if err := srv.applyAuthSnapshot(ctx, snap2); err != nil {
		t.Fatal(err)
	}
	if ka, _ := st.APIKeys().GetByHash(ctx, auth.Hash(plainA)); ka == nil || ka.Revoked {
		t.Fatalf("key a stays live across a snapshot that names limits: %+v", ka)
	}
	kb, _ := st.APIKeys().GetByHash(ctx, auth.Hash(plainB))
	if kb == nil || !kb.Revoked {
		t.Fatalf("key b must be revoked once it leaves the snapshot: %+v", kb)
	}
	kl, _ := st.APIKeys().GetByID(ctx, localRec.ID)
	if kl == nil || kl.Revoked {
		t.Fatalf("locally minted key must be untouched: %+v", kl)
	}
	if srv.requireKeys() {
		t.Fatal("requireKeys must flip back off with the snapshot")
	}
	if c := code(""); c != http.StatusOK {
		t.Fatalf("keyless again after requireKeys=false: %d", c)
	}
	if c := code(plainB); c != http.StatusOK { // keys not required: revoked key is simply ignored
		t.Fatalf("keyless mode ignores keys: %d", c)
	}
	// Twenty-five calls on a key whose old snapshot said rpmLimit 20: no 429.
	for i := 0; i < 25; i++ {
		if c := code(plainA); c != http.StatusOK {
			t.Fatalf("call %d on key a: %d — the leader must enforce no per-key limit (ADR-112)", i+1, c)
		}
	}
}

// PLAN T10.19: a key the leader already holds follows the snapshot under its
// id — a rotated hash and a moved expiry take effect — and an expiry nobody
// can read is refused, never read as "never expires".
func TestAuthSnapshotUpdatesAKnownKeyAndRefusesABadExpiry(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	oldPlain, newPlain := "sk-orc-placeholder-old", "sk-orc-placeholder-new"
	apply := func(rev string, keys ...SnapshotKey) {
		t.Helper()
		if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: rev, RequireKeys: true, Keys: keys}); err != nil {
			t.Fatal(err)
		}
	}
	apply("r1", SnapshotKey{ID: "a", Name: "team-a", Hash: auth.Hash(oldPlain), ExpiresAt: "2030-01-01T00:00:00Z"})

	// Rotate the hash and move the expiry under the same id.
	apply("r2", SnapshotKey{ID: "a", Name: "team-a", Hash: auth.Hash(newPlain), ExpiresAt: "2031-06-01T00:00:00Z"})
	if k, _ := st.APIKeys().GetByHash(ctx, auth.Hash(oldPlain)); k != nil {
		t.Fatal("the old hash still authenticates after the rotation")
	}
	k, _ := st.APIKeys().GetByHash(ctx, auth.Hash(newPlain))
	if k == nil || k.ID != snapshotKeyID("a") || !k.ExpiresAt.Equal(time.Date(2031, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("the rotated key: %+v", k)
	}

	// A bad expiry: a new key is not created, a known key keeps its row.
	apply("r3",
		SnapshotKey{ID: "a", Name: "team-a", Hash: auth.Hash(newPlain), ExpiresAt: "next tuesday"},
		SnapshotKey{ID: "b", Name: "team-b", Hash: auth.Hash("sk-orc-placeholder-b"), ExpiresAt: "31/12/2030"})
	if k, _ := st.APIKeys().GetByHash(ctx, auth.Hash("sk-orc-placeholder-b")); k != nil {
		t.Fatalf("a key with an unreadable expiry was created: %+v", k)
	}
	k, _ = st.APIKeys().GetByHash(ctx, auth.Hash(newPlain))
	if k == nil || k.Revoked || !k.ExpiresAt.Equal(time.Date(2031, 6, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("a known key with an unreadable new expiry must keep its row: %+v", k)
	}
}

// PLAN T10.19's update must not churn: a manager re-issues the same snapshot
// on a period, and a key with no expiry and one with an expiry, read back
// from the store, must compare equal — no update, no log line, no event.
func TestReapplyingTheSameSnapshotUpdatesNothing(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logs bytes.Buffer
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(&logs, nil)), nil)
	snap := &AuthSnapshot{Revision: "r1", RequireKeys: true, Keys: []SnapshotKey{
		{ID: "a", Name: "no-expiry", Hash: auth.Hash("sk-orc-placeholder-a")},
		{ID: "b", Name: "expires", Hash: auth.Hash("sk-orc-placeholder-b"), ExpiresAt: "2030-01-01T00:00:00Z"},
	}}
	if err := srv.applyAuthSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	if err := srv.applyAuthSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "auth snapshot applied") {
		t.Fatalf("the same snapshot read twice logged a change:\n%s", logs.String())
	}
}
