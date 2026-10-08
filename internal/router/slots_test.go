package router

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

// ADR-091: a worker whose requests in flight reach its slots ranks behind
// every worker with a free slot — even one with a far lower score would queue
// the request inside its engine, where no request class ranks it — and a
// worker that reports no slots is judged as before.
func TestLoadRank_FullSlotsGoLast(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{})
	slots := map[string]int{"small": 2, "big": 8}
	r.SetSlotSource(func(k string) (int, bool) { n, ok := slots[k]; return n, ok })
	r.inflight["small"], r.inflight["big"] = 2, 5
	if got := rankedOrder(r, "small", "big"); got[0] != "big" {
		t.Fatalf("a worker with every slot busy goes behind one with a free slot: %v", got)
	}
	r.inflight["small"] = 1
	if got := rankedOrder(r, "small", "big"); got[0] != "small" {
		t.Fatalf("with a free slot on both, the score decides: %v", got)
	}
	r.inflight["ungoverned"] = 50
	if got := rankedOrder(r, "ungoverned", "small"); got[0] != "small" {
		t.Fatalf("no slots reported: scored on in-flight as before: %v", got)
	}
	if r.slotsFull("ungoverned") {
		t.Fatal("a worker that reports no slots is never full")
	}
	r.inflight["small"] = 2
	if !r.slotsFullLocked("small") {
		t.Fatal("in flight = slots is full")
	}
	if r.slotsFull(gangKey("m", "g1")) {
		t.Fatal("a gang with no slot count is never full")
	}
	slots[gangKey("m", "g1")] = 1
	r.inflight[gangKey("m", "g1")] = 1
	if !r.slotsFull(gangKey("m", "g1")) || GangKey("m", "g1") != gangKey("m", "g1") {
		t.Fatal("a gang's slots live under its router key")
	}
}

// A sticky pin never lands a request on a worker whose slots are all in
// flight while another worker has a free one (ADR-091); once the pinned worker
// has a free slot again the pin decides as before.
func TestPickStickyYieldsToFullSlots(t *testing.T) {
	r, _ := drainRouter(t, store.Node{ID: "a", State: store.NodeStateReady}, store.Node{ID: "b", State: store.NodeStateReady})
	r.SetStickyTTL(time.Minute)
	r.SetSlotSource(func(k string) (int, bool) { return 1, k == "a" || k == "b" })
	ctx := auth.WithTestKey(context.Background(), &store.APIKey{UserID: "alice"})
	r.rememberSticky(ctx, "m", "a")
	r.inflight["a"] = 1
	if _, node, _ := r.pick(ctx, "m"); node != "b" {
		t.Fatalf("the pinned worker is full: want b, got %q", node)
	}
	r.inflight["a"], r.inflight["b"] = 0, 0
	if _, node, _ := r.pick(ctx, "m"); node != "a" {
		t.Fatalf("with a free slot the pin decides: want a, got %q", node)
	}
}
