package router

// A draining node takes no new work from any of the router's pickers: the
// plain pick, the revision-group split in front of it, and the hedged pick.

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

func drainRouter(t *testing.T, nodes ...store.Node) (*Router, store.Store) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	for _, n := range nodes {
		n.LastHeartbeat = time.Now()
		if n.Address == "" {
			n.Address = "192.0.2.1:8081"
		}
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
		if err := st.Placements().Upsert(ctx, store.Placement{NodeID: n.ID, ModelID: "m", Status: "ready", LastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	return New(&stubEngine{name: "local"}, st), st
}

func TestPickSkipsDrainingNode(t *testing.T) {
	r, st := drainRouter(t,
		store.Node{ID: "a", State: store.NodeStateDraining},
		store.Node{ID: "b", State: store.NodeStateReady})
	ctx := context.Background()
	// "a" is the least loaded by far — load order alone would choose it.
	r.inflight["b"] = 50
	for i := 0; i < 10; i++ {
		if _, node, _ := r.pick(ctx, "m"); node != "b" {
			t.Fatalf("pick %d chose %q, want b (a is draining)", i, node)
		}
	}
	// Every worker draining: the router has no worker to name and falls to
	// the local engine, exactly as when every worker is stale.
	if _, err := st.Nodes().SetState(ctx, "b", store.NodeStateDraining); err != nil {
		t.Fatal(err)
	}
	if _, node, _ := r.pick(ctx, "m"); node != r.localNode {
		t.Fatalf("all draining: pick chose %q, want the local fallback", node)
	}
	if _, err := st.Nodes().SetState(ctx, "a", store.NodeStateReady); err != nil {
		t.Fatal(err)
	}
	if _, node, _ := r.pick(ctx, "m"); node != "a" {
		t.Fatalf("after undrain pick chose %q, want a", node)
	}
}

// A revision whose only worker drains is a revision with no worker: its
// share goes to the rest instead of being rolled and then lost.
func TestRevisionGroupIgnoresDrainingWorkers(t *testing.T) {
	r, _ := drainRouter(t,
		store.Node{ID: "old", State: store.NodeStateReady, HardwareJSON: `{"PlanRevision":1}`},
		store.Node{ID: "canary", State: store.NodeStateDraining, HardwareJSON: `{"PlanRevision":2}`})
	r.SetRevisionWeights([]RevisionWeight{{Revision: 1, Weight: 1}, {Revision: 2, Weight: 99}})
	for i := 0; i < 50; i++ {
		if _, node, _ := r.pick(context.Background(), "m"); node != "old" {
			t.Fatalf("pick %d chose %q: the draining canary's 99%% share must go to the live revision", i, node)
		}
	}
}

func TestHedgeSkipsDrainingNode(t *testing.T) {
	r, _ := drainRouter(t,
		store.Node{ID: "a", State: store.NodeStateDraining},
		store.Node{ID: "b", State: store.NodeStateReady},
		store.Node{ID: "c", State: store.NodeStateReady})
	got := r.hedgePickWorkers(context.Background(), "m", 3)
	if len(got) != 2 {
		t.Fatalf("hedge candidates = %d, want the 2 that are not draining", len(got))
	}
	for _, c := range got {
		if c.nodeID == "a" {
			t.Fatal("the draining node is a hedge candidate")
		}
	}
}
