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

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// keyedLeader is a leader whose gateway takes keys from an auth snapshot, and
// a probe that answers the status a key gets on GET /v1/models.
func keyedLeader(t *testing.T) (*Server, store.Store, func(key string) (int, string)) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = false
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)
	call := func(key string) (int, string) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/models", nil)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	return srv, st, call
}

// TestAuthTombstoneRefusesAndSurvives: a key named in revokedKeys is refused
// even while its row is still in keys, and the refusal outlives the snapshot
// that carried it — the manager's next push, which never heard of the
// tombstone, does not bring the key back.
func TestAuthTombstoneRefusesAndSurvives(t *testing.T) {
	ctx := context.Background()
	srv, st, call := keyedLeader(t)
	plainA, plainB := "sk-orc-AAAA", "sk-orc-BBBB"
	keys := []SnapshotKey{{ID: "a", Hash: auth.Hash(plainA)}, {ID: "b", Hash: auth.Hash(plainB)}}

	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r1", RequireKeys: true, Keys: keys}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(plainA); c != http.StatusOK {
		t.Fatalf("key a before any tombstone: %d", c)
	}

	// A second writer appends a tombstone and leaves keys exactly as they were.
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r1", RequireKeys: true, Keys: keys, RevokedKeys: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(plainA); c != http.StatusUnauthorized {
		t.Fatalf("tombstoned key a must be refused while its row is still in keys: %d", c)
	}
	if c, _ := call(plainB); c != http.StatusOK {
		t.Fatalf("key b is not tombstoned: %d", c)
	}

	// The manager comes back and pushes the snapshot it would have pushed
	// anyway: key a listed, no tombstone. It stays revoked.
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r2", RequireKeys: true, Keys: keys}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(plainA); c != http.StatusUnauthorized {
		t.Fatalf("a tombstone seen once must survive a later snapshot without it: %d", c)
	}

	// A tombstone for a key this leader has never seen anchors a revoked row,
	// so the key cannot be created live by a snapshot that lists it later.
	plainC := "sk-orc-CCCC"
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r3", RequireKeys: true, Keys: keys, RevokedKeys: []string{"c"}}); err != nil {
		t.Fatal(err)
	}
	withC := append(append([]SnapshotKey{}, keys...), SnapshotKey{ID: "c", Hash: auth.Hash(plainC)})
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r4", RequireKeys: true, Keys: withC}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(plainC); c != http.StatusUnauthorized {
		t.Fatalf("a key tombstoned before it was ever listed must never be served: %d", c)
	}
	if k, _ := st.APIKeys().GetByID(ctx, SnapshotKeyPrefix+"c"); k == nil || !k.Revoked {
		t.Fatalf("tombstone row for c: %+v", k)
	}

	// A tombstone reaches only snapshot-owned rows: a locally minted key with
	// the same bare id is not the snapshot's to revoke.
	localPlain, localRec, _ := auth.Generate("local", "admin", "")
	if err := st.APIKeys().Create(ctx, localRec); err != nil {
		t.Fatal(err)
	}
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r5", RequireKeys: true, Keys: keys, RevokedKeys: []string{localRec.ID}}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(localPlain); c != http.StatusOK {
		t.Fatalf("a tombstone must not reach a locally minted key: %d", c)
	}
}

// TestAuthSnapshotStaleBound: with policy auth.maxSnapshotAgeSec set, a
// snapshot older than the bound refuses keyed traffic with 503 and a reason
// naming the stale snapshot; a fresh one serves; no bound, or no issuedAt,
// is exactly the behaviour before the field existed.
func TestAuthSnapshotStaleBound(t *testing.T) {
	ctx := context.Background()
	srv, _, call := keyedLeader(t)
	plain := "sk-orc-AAAA"
	keys := []SnapshotKey{{ID: "a", Hash: auth.Hash(plain)}}
	old := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)
	fresh := time.Now().UTC().Format(time.RFC3339)

	// Unset bound: an hour-old snapshot serves (fail-static, today's behaviour).
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r1", RequireKeys: true, Keys: keys, IssuedAt: old}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(plain); c != http.StatusOK {
		t.Fatalf("no bound set: an old snapshot keeps serving: %d", c)
	}

	// Bound set, snapshot older than it: refused, with a reason.
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p1", Auth: PolicyAuth{MaxSnapshotAgeSec: 60}})
	c, body := call(plain)
	if c != http.StatusServiceUnavailable {
		t.Fatalf("stale snapshot past the bound: want 503, got %d %s", c, body)
	}
	var e struct {
		Error struct{ Message, Type string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &e); err != nil || !strings.Contains(e.Error.Message, "auth snapshot") ||
		!strings.Contains(e.Error.Message, "maxSnapshotAgeSec") || e.Error.Type != "unavailable" {
		t.Fatalf("the refusal must say why in an OpenAI-shaped body: %s", body)
	}
	if c, _ := call(""); c != http.StatusServiceUnavailable {
		t.Fatalf("no key can be judged against a stale list, a missing one included: %d", c)
	}
	// Not demand: the refusal stays out of /loadz's counters.
	if got := srv.load.sum(&srv.load.errRing, &srv.load.errSec, time.Now()); got != 0 {
		t.Fatalf("a stale-auth refusal must not count in unavailable_1m: %d", got)
	}
	st := srv.authSnapshotState(time.Now())
	if st == nil || !st.Stale || st.AgeSec == nil || *st.AgeSec < 590 || st.MaxAgeSec != 60 || st.Revision != "r1" {
		t.Fatalf("/loadz auth_snapshot: %+v", st)
	}

	// A re-issued snapshot clears it with no restart.
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r1", RequireKeys: true, Keys: keys, IssuedAt: fresh}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(plain); c != http.StatusOK {
		t.Fatalf("fresh snapshot under the bound: %d", c)
	}

	// No issuedAt: the age is unknown and no bound is enforced on it.
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r2", RequireKeys: true, Keys: keys}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(plain); c != http.StatusOK {
		t.Fatalf("no issuedAt: today's behaviour: %d", c)
	}
	if st := srv.authSnapshotState(time.Now()); st == nil || st.AgeSec != nil || st.Stale {
		t.Fatalf("unknown age is reported as unknown: %+v", st)
	}

	// Keyless endpoint: nothing to revoke, so staleness refuses nothing.
	if err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "r3", RequireKeys: false, IssuedAt: old}); err != nil {
		t.Fatal(err)
	}
	if c, _ := call(""); c != http.StatusOK {
		t.Fatalf("keyless endpoint with a stale snapshot: %d", c)
	}
}
