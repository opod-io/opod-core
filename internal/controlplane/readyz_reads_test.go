package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// countingStore counts the fleet reads readiness makes: node lists, whole
// placement reads and per-node placement reads.
type countingStore struct {
	store.Store
	nodeLists, placementAll, placementByNode atomic.Int64
}

type countingNodes struct {
	store.NodeStore
	c *countingStore
}

func (n countingNodes) List(ctx context.Context) ([]store.Node, error) {
	n.c.nodeLists.Add(1)
	return n.NodeStore.List(ctx)
}

type countingPlacements struct {
	store.PlacementStore
	c *countingStore
}

func (p countingPlacements) All(ctx context.Context) ([]store.Placement, error) {
	p.c.placementAll.Add(1)
	return p.PlacementStore.All(ctx)
}

func (p countingPlacements) GetByNode(ctx context.Context, id string) ([]store.Placement, error) {
	p.c.placementByNode.Add(1)
	return p.PlacementStore.GetByNode(ctx, id)
}

func (c *countingStore) Nodes() store.NodeStore { return countingNodes{c.Store.Nodes(), c} }
func (c *countingStore) Placements() store.PlacementStore {
	return countingPlacements{c.Store.Placements(), c}
}
func (c *countingStore) reset() {
	c.nodeLists.Store(0)
	c.placementAll.Store(0)
	c.placementByNode.Store(0)
}
func (c *countingStore) reads() (nodes, all, byNode int64) {
	return c.nodeLists.Load(), c.placementAll.Load(), c.placementByNode.Load()
}

// A router-only leader's /readyz reads the fleet ONCE per probe — one node
// list and one placement read — in every mode: live placement, sleeping
// workers, floor-0 parked and waking, and degraded (PLAN T15.11: it was up
// to four node lists and a placement read per node, ~21 queries per probe on
// a parked endpoint).
func TestReadyzReadsTheFleetOncePerProbe(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	raw, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	cs := &countingStore{Store: raw}
	srv := NewServer(cfg, cs, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	probe := func(want int, wantMode string) {
		t.Helper()
		cs.reset()
		rec := httptest.NewRecorder()
		srv.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if rec.Code != want {
			t.Fatalf("readyz = %d %s, want %d (%s)", rec.Code, rec.Body.String(), want, wantMode)
		}
		if wantMode != "" && !strings.Contains(rec.Body.String(), `"mode":"`+wantMode+`"`) {
			t.Fatalf("readyz mode: %s, want %s", rec.Body.String(), wantMode)
		}
		if n, a, b := cs.reads(); n != 1 || a != 1 || b != 0 {
			t.Fatalf("%s: %d node lists, %d placement reads, %d per-node placement reads — want 1, 1, 0", wantMode, n, a, b)
		}
	}

	probe(http.StatusServiceUnavailable, "") // empty leader: degraded
	w1 := store.Node{ID: "w1", Hostname: "w1", State: "ready", LastHeartbeat: time.Now()}
	w2 := store.Node{ID: "w2", Hostname: "w2", State: "ready", LastHeartbeat: time.Now()}
	for _, n := range []store.Node{w1, w2} {
		if err := raw.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Placements().Upsert(ctx, store.Placement{NodeID: "w1", ModelID: "m", Status: "ready", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	probe(http.StatusOK, "router-only")
	if err := raw.Placements().SetStatus(ctx, "w1", "m", PlacementSleeping); err != nil {
		t.Fatal(err)
	}
	probe(http.StatusOK, "sleeping-workers")
	if err := raw.Placements().Delete(ctx, "w1", "m"); err != nil {
		t.Fatal(err)
	}
	srv.plan.modelID, srv.plan.present, srv.plan.zeroFloor = "m", true, true
	probe(http.StatusOK, "waking") // workers registered and awake, nothing serving
	stale := time.Now().Add(-10 * time.Minute)
	for _, id := range []string{"w1", "w2"} {
		if err := raw.Nodes().Heartbeat(ctx, id, stale, ""); err != nil {
			t.Fatal(err)
		}
	}
	probe(http.StatusOK, "sleeping") // parked: every worker gone
}
