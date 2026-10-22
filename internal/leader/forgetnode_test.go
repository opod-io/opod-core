package leader

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
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

// T25.10 · a worker that stops heartbeating is forgotten. Every replaced worker
// pod used to leave its row, its placements, ~10 map entries and three metric
// series for the leader's whole life, because nothing but an operator's
// DELETE ever removed a node. Twenty workers come and go; afterwards every
// structure holds what it held before, and the two live workers are untouched.
func TestAWorkerThatStoppedHeartbeatingIsForgotten(t *testing.T) {
	srv, _, _ := drainFixture(t) // w1 and w2, live
	ctx := context.Background()
	admin := Caller{Admin: true}
	before, err := srv.store.Nodes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const gone = 20
	for i := range gone {
		id := fmt.Sprintf("pod-%02d", i)
		if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: id, Hostname: id, Address: "http://127.0.0.1:1"}, admin, "tok"); err != nil {
			t.Fatal(err)
		}
		if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: id, LoadedModels: []string{"m"},
			Load: &engines.EngineLoad{KVUsedPct: 10, QueueDepth: 1}}, admin); err != nil {
			t.Fatal(err)
		}
		srv.nodeEngine.Store(id, nodeEngineSample{})
		metrics.SetNodeUp(id, id, true)
		metrics.SetModelLoaded("m", id, true)
		metrics.SetRouterInflight(id, 1)
		// Its pod is gone: the last heartbeat it will ever send was an hour ago.
		n, err := srv.store.Nodes().Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		n.LastHeartbeat = time.Now().Add(-time.Hour)
		if err := srv.store.Nodes().Upsert(ctx, *n); err != nil {
			t.Fatal(err)
		}
	}
	if !srv.HoldsNode("pod-00") || !seriesNamesNode(t, "pod-00") {
		t.Fatal("the fixture did not populate the maps and series; the test would prove nothing")
	}

	if n := srv.forgetDeadNodes(ctx, forgetNodeAfter); n != gone {
		t.Fatalf("forgot %d workers, want %d", n, gone)
	}
	after, err := srv.store.Nodes().List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("%d node rows after the sweep, %d before", len(after), len(before))
	}
	for i := range gone {
		id := fmt.Sprintf("pod-%02d", i)
		if srv.HoldsNode(id) {
			t.Errorf("%s is still named by a leader or router map", id)
		}
		if seriesNamesNode(t, id) {
			t.Errorf("%s still has a metric series", id)
		}
		if ps, _ := srv.store.Placements().GetByNode(ctx, id); len(ps) != 0 {
			t.Errorf("%s still has %d placements", id, len(ps))
		}
	}
	for _, id := range []string{"w1", "w2"} {
		if _, err := srv.store.Nodes().Get(ctx, id); err != nil {
			t.Errorf("the live worker %s was forgotten: %v", id, err)
		}
	}
	// The stream says what happened: forgotten after going silent, not removed
	// by an operator — a manager must not read a rollout's replaced pods as
	// machines taken away.
	evs, err := srv.store.EventLog().After(ctx, 0, 10_000)
	if err != nil {
		t.Fatal(err)
	}
	forgotten, removed := 0, 0
	for _, e := range evs {
		switch e.Type {
		case "node.forgotten":
			forgotten++
		case "node.removed":
			removed++
		}
	}
	if forgotten != gone || removed != 0 {
		t.Fatalf("events: %d node.forgotten and %d node.removed, want %d and 0", forgotten, removed, gone)
	}
	// A second sweep finds nothing; a worker that comes back re-registers.
	if n := srv.forgetDeadNodes(ctx, forgetNodeAfter); n != 0 {
		t.Fatalf("a second sweep forgot %d", n)
	}
}

// seriesNamesNode reports whether any metric series is labelled with the node.
func seriesNamesNode(t *testing.T, node string) bool {
	t.Helper()
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "node" && l.GetValue() == node {
					return true
				}
			}
		}
	}
	return false
}
