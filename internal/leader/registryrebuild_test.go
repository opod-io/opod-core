package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/gateway"
	"github.com/opod-io/opod/internal/store"
)

// A managed leader keeps its worker registry in memory, so every restart
// starts with none and its workers come back one heartbeat at a time. Until
// they have, GET /admin/v1/nodes says the list is still being rebuilt, so a
// front door does not read "no workers" as "every worker is gone" (T32.48).
//
// The clock starts at the first worker that registers, not at boot: on the
// design-partner cell the workers reached a restarted leader 20 s after its
// process started, and a window counted from boot had ended before any of
// them was back (one 503 in three drills).
func TestALeaderRebuildsUntilThreeHeartbeatsAfterTheFirstWorkerReturns(t *testing.T) {
	boot := time.Now()
	empty, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { empty.Close() })
	r := newRegistryRebuild(empty, boot)

	if !r.active(boot.Add(30 * time.Second)) {
		t.Fatal("no worker has come back yet, 30 s after boot: the list is still incomplete")
	}
	first := boot.Add(20 * time.Second) // what the cell showed
	r.joined(first)
	r.joined(first.Add(4 * time.Second)) // later joins do not move the clock
	if !r.active(first.Add(registryRebuildWindow - time.Second)) {
		t.Fatal("within three heartbeats of the first worker's return, others may still be on their way")
	}
	if r.active(first.Add(registryRebuildWindow)) {
		t.Fatal("three heartbeats after the first worker returned, the list is whole")
	}
	if registryRebuildWindow < 2*5*time.Second {
		t.Fatalf("the window (%v) must outlast more than one worker heartbeat", registryRebuildWindow)
	}

	lonely := newRegistryRebuild(empty, boot)
	if lonely.active(boot.Add(registryRebuildCap)) {
		t.Fatal("a leader no worker reaches stops saying rebuilding at the cap, or a real removal is held back for ever")
	}

	known, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { known.Close() })
	if err := known.Nodes().Upsert(context.Background(), store.Node{ID: "n_a", Hostname: "a", Address: "192.0.2.1:8081", State: "ready", LastHeartbeat: boot}); err != nil {
		t.Fatal(err)
	}
	if newRegistryRebuild(known, boot).active(boot) {
		t.Fatal("a leader that booted with its workers in the store is not rebuilding")
	}
	var none *registryRebuild
	none.joined(boot)
	if none.active(boot) {
		t.Fatal("a server built without the tracker is never rebuilding")
	}

	// End to end through the handler: a fresh leader says so; after its first
	// worker plus the window, it does not.
	cfg := config.Default()
	cfg.Auth.RequireKeys = false
	srv := NewServer(cfg, empty, &downEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	header := func() string {
		rec := httptest.NewRecorder()
		srv.listNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/nodes", nil))
		return rec.Header().Get(gateway.RegistryHeader)
	}
	if got := header(); got != gateway.RegistryRebuilding {
		t.Fatalf("a freshly booted leader's node list must say %s: %s, got %q", gateway.RegistryHeader, gateway.RegistryRebuilding, got)
	}
	if _, err := srv.RegisterNode(context.Background(), RegisterRequest{ID: "w1", Hostname: "w1", Address: "192.0.2.2:8081"}, Caller{Admin: true}, "tok"); err != nil {
		t.Fatal(err)
	}
	srv.rebuild.firstJoin = time.Now().Add(-registryRebuildWindow) // the window has passed
	if got := header(); got != "" {
		t.Fatalf("once rebuilt the list must not say rebuilding, got %q", got)
	}
}
