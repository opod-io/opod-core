package controlplane

import (
	"context"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// A removed node leaves nothing behind on the leader or the router: not its
// load or engine samples, not its reconcile or silence marks, not the driver
// of a gang it coordinated, and none of the router's per-node maps (PLAN
// T15.4 — seven maps that only ever grew).
func TestRemoveNodeForgetsEverythingAboutIt(t *testing.T) {
	srv, _, _ := drainFixture(t) // w1 and w2 registered and serving "m"
	ctx := context.Background()
	admin := Caller{Admin: true}
	hb := HeartbeatRequest{ID: "w1", LoadedModels: []string{"m"},
		Load: &engines.EngineLoad{KVUsedPct: 50, QueueDepth: 1}}
	if err := srv.HeartbeatNode(ctx, hb, admin); err != nil {
		t.Fatal(err)
	}
	srv.nodeEngine.Store("w1", nodeEngineSample{})
	srv.reconcileNodes.Store("w1", struct{}{})
	srv.engineSilentTold.Store("w1", struct{}{})
	// A gang w1 coordinates, with its driver built and a sample cached.
	coord := store.Shard{ID: "shard:m:g1:0", ModelID: "m", NodeID: "w1", Role: "coordinator", Address: "127.0.0.1:1"}
	if err := srv.store.Shards().Create(ctx, coord); err != nil {
		t.Fatal(err)
	}
	if _, ok := srv.gangEngine(coord); !ok {
		t.Fatal("gang engine")
	}
	// Swap the built driver for one that reports its release (T15.16).
	pool := &poolEngine{}
	srv.gangEng.Store(coord.ID, gangCoordEngine{addr: coord.Address, eng: pool})
	srv.gangLoad.Store(coord.ID, nodeLoadSample{})
	// The router has per-node state too.
	srv.router.InvalidateNode("") // no-op, proves the path is callable
	if !srv.HoldsNode("w1") {
		t.Fatal("the fixture did not populate the leader's maps; the test would prove nothing")
	}

	if err := srv.RemoveNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if srv.HoldsNode("w1") {
		t.Error("a removed node is still named by a leader or router map")
	}
	if _, ok := srv.gangEng.Load(coord.ID); ok {
		t.Error("the removed node's gang driver survived")
	}
	if pool.released != 1 {
		t.Errorf("the removed node's gang driver released its pool %d times, want 1", pool.released)
	}
	if _, ok := srv.gangLoad.Load(coord.ID); ok {
		t.Error("the removed node's gang sample survived")
	}
	// The other node is untouched.
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w2", LoadedModels: []string{"m"}, Load: &engines.EngineLoad{}}, admin); err != nil {
		t.Fatal(err)
	}
	if !srv.HoldsNode("w2") {
		t.Error("a live node's samples were dropped")
	}
}

// poolEngine is an engine that knows whether its connection pool was released.
type poolEngine struct {
	engines.Engine
	released int
}

func (p *poolEngine) CloseIdleConnections() { p.released++ }

// A gang that is no longer a shard row loses its cached driver on the next
// /loadz assembly; one that is merely unroutable keeps it (the driver holds
// the tokens_per_s rate between samples).
func TestGoneGangsAreForgottenOnTheNextLoadRead(t *testing.T) {
	srv, _, _ := drainFixture(t)
	ctx := context.Background()
	kept := store.Shard{ID: "shard:m:g1:0", ModelID: "m", NodeID: "w1", Role: "coordinator", Address: "127.0.0.1:1"}
	if err := srv.store.Shards().Create(ctx, kept); err != nil {
		t.Fatal(err)
	}
	gone := store.Shard{ID: "shard:m:g0:0", ModelID: "m", NodeID: "w2", Role: "coordinator", Address: "127.0.0.1:2"}
	for _, c := range []store.Shard{kept, gone} {
		if _, ok := srv.gangEngine(c); !ok {
			t.Fatal("gang engine")
		}
	}
	srv.gangCoordinators(ctx, "m")
	if _, ok := srv.gangEng.Load(gone.ID); ok {
		t.Error("a gang with no shard row kept its driver")
	}
	if _, ok := srv.gangEng.Load(kept.ID); !ok {
		t.Error("a gang that is still a shard row lost its driver")
	}
}
