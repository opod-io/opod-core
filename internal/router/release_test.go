package router

import (
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// poolEngine is an engine that knows whether its connection pool was released.
type poolEngine struct {
	engines.Engine
	released int
}

func (p *poolEngine) CloseIdleConnections() { p.released++ }

// An evicted engine's pool is released at the eviction, not at the transport's
// idle timeout (PLAN T15.16): on a removed node, on a torn-down model's gangs,
// and never on a client the router still dials.
func TestEvictedEnginesReleaseTheirPools(t *testing.T) {
	r := New(nil, nil)
	node, gangA, gangB, other := &poolEngine{}, &poolEngine{}, &poolEngine{}, &poolEngine{}
	r.remotes["n1"] = node
	r.remotes["shard:m:g1"] = gangA
	r.remotes["shard:m:g2"] = gangB
	r.remotes["shard:other:g1"] = other

	r.InvalidateNode("n1")
	if node.released != 1 {
		t.Fatalf("a removed node's client released %d times, want 1", node.released)
	}
	r.InvalidateModel("m")
	if gangA.released != 1 || gangB.released != 1 {
		t.Fatalf("a torn-down model's gang clients released %d/%d times, want 1/1", gangA.released, gangB.released)
	}
	if other.released != 0 {
		t.Fatal("a live model's client was released")
	}
	if _, ok := r.remotes["shard:other:g1"]; !ok {
		t.Fatal("a live model's client was evicted")
	}
}
