package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/store"
)

// One rule for "which rows are workers" (WorkerFor), asked by the leader's own
// pick exactly as by the CLI's picker: the leader's "local" row is never one,
// however much RAM it reports, and neither is a draining or a silent node.
func TestPickWorkersAsksTheWorkerRule(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, now := context.Background(), time.Now()
	for _, n := range []store.Node{
		{ID: "local", RAMGB: 2048, Address: "127.0.0.1:8080", State: "ready", LastHeartbeat: now},
		{ID: "small", RAMGB: 32, Address: "192.0.2.1:8081", State: "ready", LastHeartbeat: now},
		{ID: "big", RAMGB: 256, Address: "192.0.2.2:8081", State: "ready", LastHeartbeat: now},
		{ID: "drained", RAMGB: 512, Address: "192.0.2.3:8081", State: "draining", LastHeartbeat: now},
		{ID: "silent", RAMGB: 512, Address: "192.0.2.4:8081", State: "ready", LastHeartbeat: now.Add(-time.Hour)},
		{ID: "nowhere", RAMGB: 512, State: "ready", LastHeartbeat: now},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	got, err := o.pickWorkers(ctx, 2)
	if err != nil || len(got) != 2 || got[0].ID != "big" || got[1].ID != "small" {
		t.Fatalf("pickWorkers(2) = %+v, %v; want big, small", got, err)
	}
	if _, err := o.pickWorkers(ctx, 3); err == nil || !strings.Contains(err.Error(), "need 3 ready workers, have 2") {
		t.Fatalf("pickWorkers(3): %v", err)
	}
	for _, id := range []string{"local", "drained", "silent", "nowhere"} {
		if _, err := o.pickWorkersByID(ctx, []string{"big", id}); err == nil || !strings.Contains(err.Error(), id) {
			t.Errorf("naming %q: %v, want a refusal that names it", id, err)
		}
	}
	// The CLI's picker sees the same two.
	facts, err := WorkerMemoryFacts(ctx, st, nil, "", 20, 0, now)
	if err != nil || len(facts) != 2 {
		t.Fatalf("WorkerMemoryFacts = %+v, %v; want the same two workers", facts, err)
	}
}

// The coordinator holds the KV cache, so it belongs on the biggest CARD — not
// the biggest host. Two hosts alike in RAM and unalike in cards is the ordinary
// mixed fleet, and it is what broke on the design-partner cell (2026-09-20):
// both reported 31 GB of RAM behind a 46 GB card and an 8 GB card, the tie fell
// to whichever came first, and llama.cpp died allocating the KV cache on the
// small one — "failed to allocate RPC0 buffer of size 4294967296".
func TestCoordinatorGoesOnTheBiggestCardNotTheBiggestHost(t *testing.T) {
	hw := func(vramGB int) string {
		raw, err := json.Marshal(agent.Capabilities{GPUs: []agent.GPU{{Name: "card", VRAMGB: vramGB}}})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	o := &Orchestrator{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx := context.Background()

	small := store.Node{ID: "small-card", RAMGB: 31, HardwareJSON: hw(8)}
	big := store.Node{ID: "big-card", RAMGB: 31, HardwareJSON: hw(46)}

	// Whichever order they arrive in, the big card wins.
	if got := o.pickCoordinatorHost(ctx, []store.Node{small, big}); got.nodeID != "big-card" {
		t.Errorf("coordinator = %q, want big-card", got.nodeID)
	}
	if got := o.pickCoordinatorHost(ctx, []store.Node{big, small}); got.nodeID != "big-card" {
		t.Errorf("coordinator = %q with the order reversed, want big-card", got.nodeID)
	}

	// A host with MORE RAM and a SMALLER card does not win: RAM is not what
	// runs out here.
	roomy := store.Node{ID: "roomy-host", RAMGB: 512, HardwareJSON: hw(8)}
	if got := o.pickCoordinatorHost(ctx, []store.Node{roomy, big}); got.nodeID != "big-card" {
		t.Errorf("coordinator = %q, want big-card — host RAM is not the constraint", got.nodeID)
	}

	// Workers that report no card at all fall back to host RAM, as before.
	cpuBig := store.Node{ID: "cpu-big", RAMGB: 256}
	cpuSmall := store.Node{ID: "cpu-small", RAMGB: 32}
	if got := o.pickCoordinatorHost(ctx, []store.Node{cpuSmall, cpuBig}); got.nodeID != "cpu-big" {
		t.Errorf("coordinator = %q among cardless workers, want cpu-big", got.nodeID)
	}

	// An explicit head still wins over any of it (feature shard_head).
	o.CoordinatorNode = "small-card"
	if got := o.pickCoordinatorHost(ctx, []store.Node{small, big}); got.nodeID != "small-card" {
		t.Errorf("a named head must be honoured, got %q", got.nodeID)
	}
}
