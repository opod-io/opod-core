package router

// "Live" means one thing to every step of the pick: the revision split sees
// only workers the walk would actually choose.

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// A canary whose only worker stopped heartbeating (the row is still there)
// must not keep its weight: those requests used to win the roll, find no live
// worker in the group and fall to the local fallback. Its share goes to the
// other revision, nothing falls through, and it comes back with the worker.
func TestARevisionWhoseOnlyWorkerIsLostGetsNoTraffic(t *testing.T) {
	r, st := drainRouter(t,
		store.Node{ID: "old", State: store.NodeStateReady, HardwareJSON: `{"PlanRevision":1}`},
		store.Node{ID: "canary", State: store.NodeStateReady, HardwareJSON: `{"PlanRevision":2}`})
	r.SetHeartbeatMaxAge(30 * time.Second)
	r.SetRevisionWeights([]RevisionWeight{{Revision: 1, Weight: 1}, {Revision: 2, Weight: 99}})
	ctx := context.Background()

	// Both live: the canary's 99 % is real.
	seen := map[string]int{}
	for i := 0; i < 200; i++ {
		_, node, _ := r.pick(ctx, "m")
		seen[node]++
	}
	if seen["canary"] < 150 || seen[r.localNode] != 0 {
		t.Fatalf("both revisions live, 99:1 → %v", seen)
	}

	// The canary's pod dies: heartbeats stop, the row stays.
	if err := st.Nodes().Heartbeat(ctx, "canary", time.Now().Add(-5*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, node, _ := r.pick(ctx, "m"); node != "old" {
			t.Fatalf("pick %d chose %q: a revision whose only worker is lost must give its share to the other, and nothing may fall to the local fallback", i, node)
		}
	}

	// It heartbeats again: its share is back.
	if err := st.Nodes().Heartbeat(ctx, "canary", time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	seen = map[string]int{}
	for i := 0; i < 200; i++ {
		_, node, _ := r.pick(ctx, "m")
		seen[node]++
	}
	if seen["canary"] < 150 {
		t.Fatalf("the canary heartbeats again, 99:1 → %v", seen)
	}
}

// The same for a worker in the router's penalty box: a revision whose only
// worker is cooling down has no worker to send its share to.
func TestARevisionWhoseOnlyWorkerCoolsDownGetsNoTraffic(t *testing.T) {
	r, _ := drainRouter(t,
		store.Node{ID: "old", State: store.NodeStateReady, HardwareJSON: `{"PlanRevision":1}`},
		store.Node{ID: "canary", State: store.NodeStateReady, HardwareJSON: `{"PlanRevision":2}`})
	r.SetRevisionWeights([]RevisionWeight{{Revision: 1, Weight: 1}, {Revision: 2, Weight: 99}})
	r.SetPlacementCooldown(1, time.Minute)
	r.mu.Lock()
	r.cooldowns["canary"] = time.Now().Add(time.Minute)
	r.mu.Unlock()
	for i := 0; i < 50; i++ {
		if _, node, _ := r.pick(context.Background(), "m"); node != "old" {
			t.Fatalf("pick %d chose %q, want old", i, node)
		}
	}
}
