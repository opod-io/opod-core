package router

// Multi-head routing (build item 6): a sharded model may have SEVERAL gangs,
// each a complete copy of the weights that serves whole requests. The router
// picks among their coordinators the way it picks among workers.
//
// Every test here is really one assertion in different clothes: a judgement
// about a gang must be made from THAT gang's parts. Made over the union of a
// model's parts — which is what the code did when a model could only have one
// gang — one gang's fault took its healthy siblings down, and one gang's ready
// coordinator vouched for a broken one.

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// gangRouter builds a leader whose store holds the given gangs. Each entry is
// a gang id and the node ids of its coordinator and one rpc part.
func gangRouter(t *testing.T, model string, gangs map[string][2]string, nodes ...store.Node) (*Router, store.Store) {
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
	}
	for gang, pair := range gangs {
		coordNode, rpcNode := pair[0], pair[1]
		parts := []store.Shard{
			{ID: "s-" + model + "-" + gang + "-coord", ModelID: model, GangID: gang, Role: "coordinator",
				NodeID: coordNode, Address: coordNode + ":9001", ProcessID: "p-" + gang + "-c", Status: "ready"},
			{ID: "s-" + model + "-" + gang + "-rpc-0", ModelID: model, GangID: gang, Role: "rpc",
				NodeID: rpcNode, Address: rpcNode + ":50052", ProcessID: "p-" + gang + "-r", Status: "ready"},
		}
		for _, p := range parts {
			p.CreatedAt, p.LastSeen = time.Now(), time.Now()
			if err := st.Shards().Create(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	r := New(&stubEngine{name: "local"}, st)
	r.heartbeatMaxAge = time.Minute
	return r, st
}

// The pick must land on a gang, and the node id it is accounted under must
// name that gang — otherwise two gangs' in-flight counts are one number and
// the load-aware pick below cannot tell them apart.
func TestPickRoutesToAGangAndNamesIt(t *testing.T) {
	r, _ := gangRouter(t, "m", map[string][2]string{"g0": {"n1", "n2"}},
		store.Node{ID: "n1"}, store.Node{ID: "n2"})
	_, node, err := r.pick(context.Background(), "m")
	if err != nil {
		t.Fatal(err)
	}
	if node != "shard:m:g0" {
		t.Fatalf("the pick must name the gang it chose, got %q", node)
	}
}

// The whole point of a second gang: it carries requests. The least loaded gang
// wins, exactly as the least loaded worker does.
func TestPickPrefersTheLeastLoadedGang(t *testing.T) {
	r, _ := gangRouter(t, "m", map[string][2]string{"g0": {"n1", "n2"}, "g1": {"n3", "n4"}},
		store.Node{ID: "n1"}, store.Node{ID: "n2"}, store.Node{ID: "n3"}, store.Node{ID: "n4"})
	ctx := context.Background()
	r.inflight["shard:m:g0"] = 9 // g0 is busy; g1 is idle
	for i := 0; i < 5; i++ {
		if _, node, _ := r.pick(ctx, "m"); node != "shard:m:g1" {
			t.Fatalf("the idle gang must take the request, got %q", node)
		}
	}
	// And the other way round, so the result is the load and not the ordering.
	r.inflight["shard:m:g0"] = 0
	r.inflight["shard:m:g1"] = 9
	if _, node, _ := r.pick(ctx, "m"); node != "shard:m:g0" {
		t.Fatalf("load, not gang order, decides: got %q", node)
	}
}

// A gang whose part sits on a drained node is out of rotation — and ONLY that
// gang. Over the union of the model's parts this drain took every gang out,
// so a model with a healthy spare copy served nothing.
func TestOneGangsLostPartLeavesTheOtherServing(t *testing.T) {
	r, _ := gangRouter(t, "m", map[string][2]string{"g0": {"n1", "n2"}, "g1": {"n3", "n4"}},
		store.Node{ID: "n1"}, store.Node{ID: "n2", State: store.NodeStateDraining},
		store.Node{ID: "n3"}, store.Node{ID: "n4"})
	ctx := context.Background()
	// g0's rpc part is on the draining node; g1 is untouched.
	for i := 0; i < 5; i++ {
		_, node, err := r.pick(ctx, "m")
		if err != nil {
			t.Fatalf("the healthy gang must still serve: %v", err)
		}
		if node != "shard:m:g1" {
			t.Fatalf("a drained part takes ONLY its own gang out, got %q", node)
		}
	}
}

// With every gang out, the model is not routable through the shard path at
// all — one gang's ready coordinator must not vouch for the others.
func TestEveryGangDownIsNotRoutable(t *testing.T) {
	r, _ := gangRouter(t, "m", map[string][2]string{"g0": {"n1", "n2"}, "g1": {"n3", "n4"}},
		store.Node{ID: "n1"}, store.Node{ID: "n2", State: store.NodeStateDraining},
		store.Node{ID: "n3"}, store.Node{ID: "n4", State: store.NodeStateDraining})
	if _, _, ok := r.shardCoordinator(context.Background(), "m"); ok {
		t.Fatal("no gang is whole, so the shard path must not offer a route")
	}
}

// Invalidation must drop EVERY gang's cached engine. Dropping one key left the
// others dialling a coordinator that a create had just moved or removed.
func TestInvalidateModelDropsEveryGang(t *testing.T) {
	r, _ := gangRouter(t, "m", map[string][2]string{"g0": {"n1", "n2"}, "g1": {"n3", "n4"}},
		store.Node{ID: "n1"}, store.Node{ID: "n2"}, store.Node{ID: "n3"}, store.Node{ID: "n4"})
	ctx := context.Background()
	r.inflight["shard:m:g0"] = 5 // make the pick visit g1 as well
	_, _, _ = r.pick(ctx, "m")
	r.inflight["shard:m:g0"] = 0
	r.inflight["shard:m:g1"] = 5
	_, _, _ = r.pick(ctx, "m")
	if len(r.remotes) == 0 {
		t.Fatal("the picks should have cached at least one gang engine")
	}
	r.InvalidateModel("m")
	for key := range r.remotes {
		if len(key) > 8 && key[:8] == "shard:m:" {
			t.Fatalf("gang engine %q survived invalidation", key)
		}
	}
}
