package leader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/router"
	"github.com/opod-io/opod-core/internal/store"
)

// T28.8: a gang's answer carries a route token too. It names the gang and its
// head worker, and holds only while that head is the same process: a head
// that restarted, or a gang re-formed on another head, is a miss (ADR-042).
func TestAGangsRouteTokenIsAMissOnceItsHeadRestarted(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model := "org:model" // a ':' in the id: the key is rebuilt, never parsed
	for _, n := range []store.Node{
		{ID: "h1", Hostname: "h1", State: "ready", BootID: "boot-1", LastHeartbeat: time.Now()},
		{ID: "h2", Hostname: "h2", State: "ready", BootID: "boot-9", LastHeartbeat: time.Now()},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	coord := store.Shard{ID: "c", ModelID: model, GangID: "g0", Role: "coordinator", NodeID: "h1", Address: "h1:9100",
		ProcessID: "p", Status: "ready", CreatedAt: time.Now(), LastSeen: time.Now()}
	if err := st.Shards().Create(ctx, coord); err != nil {
		t.Fatal(err)
	}
	srv := sessionDoor(t, st)
	key := router.GangKey(model, "g0")

	r1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r1.Header.Set(SessionHeader, "conv-7")
	r1 = srv.withSession(r1, nil, model)
	r1 = r1.WithContext(router.WithTrace(r1.Context()))
	router.NoteGang(r1.Context(), model, "g0", "h1")
	router.NoteNode(r1.Context(), key)
	rec := httptest.NewRecorder()
	srv.wrapRoute(rec, r1, model).WriteHeader(http.StatusOK)
	token := rec.Header().Get(SessionRouteHeader)
	if token == "" {
		t.Fatal("a gang's 200 answer to a request with a session carried no route token")
	}
	sess := router.SessionFrom(r1.Context())
	if got := srv.verifyRoute(ctx, token, sess, model, time.Now()); got != key {
		t.Fatalf("the token reads as %q, want the gang's key %q", got, key)
	}

	// The head restarted: same node, a new boot id.
	_ = st.Nodes().Upsert(ctx, store.Node{ID: "h1", Hostname: "h1", State: "ready", BootID: "boot-2", LastHeartbeat: time.Now()})
	if got := srv.verifyRoute(ctx, token, sess, model, time.Now()); got != "" {
		t.Fatalf("a token minted before the head restarted still routes (%q)", got)
	}

	// The gang re-formed with its head on another worker: a miss, though the
	// token's own worker is back on its old boot id.
	_ = st.Nodes().Upsert(ctx, store.Node{ID: "h1", Hostname: "h1", State: "ready", BootID: "boot-1", LastHeartbeat: time.Now()})
	if err := st.Shards().DeleteByModel(ctx, model); err != nil {
		t.Fatal(err)
	}
	coord.NodeID, coord.ID = "h2", "c2"
	if err := st.Shards().Create(ctx, coord); err != nil {
		t.Fatal(err)
	}
	if got := srv.verifyRoute(ctx, token, sess, model, time.Now()); got != "" {
		t.Fatalf("a token for a gang re-formed on another head still routes (%q)", got)
	}
}
