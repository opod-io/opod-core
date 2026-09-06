package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

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
		{ID: "a", Name: "team-a", Hash: auth.Hash(plainA), RPMLimit: 10},
		{ID: "b", Name: "team-b", Hash: auth.Hash(plainB), AllowedModels: []string{"m1"}},
	}}
	if err := srv.applyAuthSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if !srv.requireKeys() {
		t.Fatal("requireKeys must follow the snapshot")
	}
	ka, _ := st.APIKeys().GetByHash(ctx, auth.Hash(plainA))
	if ka == nil || ka.ID != SnapshotKeyPrefix+"a" || ka.RPMLimit != 10 || ka.Scope != "user" {
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

	// second snapshot: limits change, key b leaves, requireKeys off
	snap2 := &AuthSnapshot{Revision: "r2", RequireKeys: false, Keys: []SnapshotKey{
		{ID: "a", Name: "team-a", Hash: auth.Hash(plainA), RPMLimit: 20, TPMLimit: 5},
	}}
	if err := srv.applyAuthSnapshot(ctx, snap2); err != nil {
		t.Fatal(err)
	}
	ka, _ = st.APIKeys().GetByHash(ctx, auth.Hash(plainA))
	if ka.RPMLimit != 20 || ka.TPMLimit != 5 {
		t.Fatalf("limits not updated: %+v", ka)
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
}
