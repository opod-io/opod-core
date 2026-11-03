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
// front door does not read "no workers" as "every worker is gone" (T32.48). A
// leader that booted with workers in its store is not rebuilding.
func TestALeaderThatBootsWithNoWorkersSaysItIsRebuilding(t *testing.T) {
	now := time.Now()
	empty, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { empty.Close() })
	if until := registryRebuildUntil(empty, now); !until.Equal(now.Add(registryRebuildWindow)) {
		t.Fatalf("an empty registry at boot: rebuilding until %v, want %v", until, now.Add(registryRebuildWindow))
	}
	if registryRebuildWindow < 2*5*time.Second {
		t.Fatalf("the window (%v) must outlast more than one worker heartbeat, or a worker whose first one was lost reads as gone", registryRebuildWindow)
	}

	known, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { known.Close() })
	if err := known.Nodes().Upsert(context.Background(), store.Node{ID: "n_a", Hostname: "a", Address: "192.0.2.1:8081", State: "ready", LastHeartbeat: now}); err != nil {
		t.Fatal(err)
	}
	if until := registryRebuildUntil(known, now); !until.IsZero() {
		t.Fatalf("a leader that booted with its workers in the store is not rebuilding (until %v)", until)
	}

	cfg := config.Default()
	cfg.Auth.RequireKeys = false
	srv := NewServer(cfg, empty, &downEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	rec := httptest.NewRecorder()
	srv.listNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/nodes", nil))
	if got := rec.Header().Get(gateway.RegistryHeader); got != gateway.RegistryRebuilding {
		t.Fatalf("a freshly booted leader's node list must say %s: %s, got %q", gateway.RegistryHeader, gateway.RegistryRebuilding, got)
	}
	srv.rebuildingUntil = time.Now().Add(-time.Second) // the window has passed
	rec = httptest.NewRecorder()
	srv.listNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/nodes", nil))
	if got := rec.Header().Get(gateway.RegistryHeader); got != "" {
		t.Fatalf("after the window the list is whole and must not say rebuilding, got %q", got)
	}
}
