package router

// R9.4: load-aware and prefix-affine pick order, against a fake load table.

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

func rankedOrder(r *Router, ids ...string) []string {
	workers := make([]store.Placement, 0, len(ids))
	for _, id := range ids {
		workers = append(workers, store.Placement{NodeID: id})
	}
	r.mu.RLock()
	type ranked struct {
		sat bool
		sc  float64
	}
	rank := map[string]ranked{}
	for _, w := range workers {
		sat, sc := r.loadRank(w.NodeID)
		rank[w.NodeID] = ranked{sat, sc}
	}
	r.mu.RUnlock()
	// the same comparator pick() uses
	for i := 1; i < len(workers); i++ {
		for j := i; j > 0; j-- {
			a, b := rank[workers[j-1].NodeID], rank[workers[j].NodeID]
			less := false
			if a.sat != b.sat {
				less = !b.sat
			} else {
				less = b.sc < a.sc
			}
			if less {
				workers[j-1], workers[j] = workers[j], workers[j-1]
			}
		}
	}
	out := make([]string, 0, len(workers))
	for _, w := range workers {
		out = append(out, w.NodeID)
	}
	return out
}

func newLoadRouter(table map[string]LoadSignal) *Router {
	r := &Router{inflight: map[string]int{}, stickiness: map[string]stickyEntry{}}
	r.SetLoadSource(func(id string) (LoadSignal, bool) { s, ok := table[id]; return s, ok })
	return r
}

// With the weights off the order is the old one: in-flight only, even when
// the samples say a worker is full.
func TestLoadRank_OffIsInflightOnly(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{"full": {KVUsedPct: 99}, "idle": {KVUsedPct: 5}})
	r.inflight["full"], r.inflight["idle"] = 1, 2
	if got := rankedOrder(r, "idle", "full"); got[0] != "full" {
		t.Fatalf("weights off → in-flight order: %v", got)
	}
}

// The queue and the KV weight join the score.
func TestLoadRank_KVWeightAndQueue(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{"a": {KVUsedPct: 80, QueueDepth: 0}, "b": {KVUsedPct: 10, QueueDepth: 0}, "c": {KVUsedPct: 10, QueueDepth: 3}})
	r.SetLoadAware(4, 0, false) // a full cache = 4 requests
	r.inflight["a"], r.inflight["b"], r.inflight["c"] = 1, 2, 1
	// a: 1 + 4×0.8 = 4.2 · b: 2 + 0.4 = 2.4 · c: 1 + 3 + 0.4 = 4.4
	if got := rankedOrder(r, "a", "b", "c"); got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Fatalf("score order b < a < c: %v", got)
	}
}

// A saturated worker stops receiving new requests before it errors: it goes
// behind every worker with headroom, whatever its in-flight count.
func TestLoadRank_SaturatedGoesLast(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{"full": {KVUsedPct: 97}, "busy": {KVUsedPct: 60}})
	r.SetLoadAware(0, 95, false)
	r.inflight["full"], r.inflight["busy"] = 0, 5
	if got := rankedOrder(r, "full", "busy"); got[0] != "busy" {
		t.Fatalf("saturated last: %v", got)
	}
	// Every worker saturated → the least loaded of them.
	r2 := newLoadRouter(map[string]LoadSignal{"x": {KVUsedPct: 99}, "y": {KVUsedPct: 96}})
	r2.SetLoadAware(0, 95, false)
	r2.inflight["x"], r2.inflight["y"] = 3, 1
	if got := rankedOrder(r2, "x", "y"); got[0] != "y" {
		t.Fatalf("all saturated → least loaded: %v", got)
	}
	// No sample = no saturation, scored on in-flight alone.
	r3 := newLoadRouter(map[string]LoadSignal{})
	r3.SetLoadAware(4, 95, false)
	r3.inflight["p"], r3.inflight["q"] = 2, 1
	if got := rankedOrder(r3, "p", "q"); got[0] != "q" {
		t.Fatalf("unsampled → in-flight: %v", got)
	}
}

// The prefix key is a hash of the system prompt and the opening user turn,
// never the prompt; a different opening turn is a different key; a long
// document does not decide the key.
func TestPrefixKey(t *testing.T) {
	a := engines.ChatRequest{Messages: []engines.Message{{Role: "system", Content: "You are terse."}, {Role: "user", Content: "hello"}, {Role: "assistant", Content: "hi"}, {Role: "user", Content: "more"}}}
	b := engines.ChatRequest{Messages: []engines.Message{{Role: "system", Content: "You are terse."}, {Role: "user", Content: "hello"}}}
	c := engines.ChatRequest{Messages: []engines.Message{{Role: "system", Content: "You are terse."}, {Role: "user", Content: "goodbye"}}}
	if prefixKeyOf(a) == "" || prefixKeyOf(a) != prefixKeyOf(b) {
		t.Fatalf("same opening → same key: %q %q", prefixKeyOf(a), prefixKeyOf(b))
	}
	if prefixKeyOf(a) == prefixKeyOf(c) {
		t.Fatal("different opening turn → different key")
	}
	if prefixKeyOf(engines.ChatRequest{}) != "" {
		t.Fatal("nothing to key on → empty")
	}
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: string(make([]byte, 5000))}}}
	long2 := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: string(make([]byte, 6000))}}}
	if prefixKeyOf(long) != prefixKeyOf(long2) {
		t.Fatal("the key is the capped head, not the whole document")
	}
}

// Affinity: off by default; on, a served prefix pins to its worker for the
// TTL and the pin is consulted for the same model only.
func TestPrefixAffinity(t *testing.T) {
	r := newLoadRouter(nil)
	ctx := withPrefixKey(context.Background(), "abc")
	r.rememberPrefix(ctx, "m", "w1")
	if got := r.prefixPick(ctx, "m"); got != "" {
		t.Fatalf("affinity off → no pin: %q", got)
	}
	r.SetLoadAware(0, 0, true)
	r.rememberPrefix(ctx, "m", "w1")
	if got := r.prefixPick(ctx, "m"); got != "w1" {
		t.Fatalf("pinned: %q", got)
	}
	if got := r.prefixPick(ctx, "other"); got != "" {
		t.Fatalf("per model: %q", got)
	}
	if got := r.prefixPick(context.Background(), "m"); got != "" {
		t.Fatalf("no key → no pin: %q", got)
	}
	r.SetStickyTTL(time.Millisecond)
	r.rememberPrefix(ctx, "m", "w1")
	time.Sleep(5 * time.Millisecond)
	if got := r.prefixPick(ctx, "m"); got != "" {
		t.Fatalf("expired: %q", got)
	}
}

// pick() prefers the pinned worker only while it has headroom.
func TestPrefixPinYieldsToSaturation(t *testing.T) {
	table := map[string]LoadSignal{"w1": {KVUsedPct: 99}, "w2": {KVUsedPct: 10}}
	r := newLoadRouter(table)
	r.SetLoadAware(0, 95, true)
	ctx := withPrefixKey(context.Background(), "k")
	r.rememberPrefix(ctx, "m", "w1")
	r.mu.RLock()
	sat, _ := r.loadRank("w1")
	r.mu.RUnlock()
	if !sat {
		t.Fatal("w1 is saturated")
	}
	workers := []store.Placement{{NodeID: "w2"}, {NodeID: "w1"}}
	if pin := r.prefixPick(ctx, "m"); pin != "" && !sat {
		workers = preferNode(workers, pin)
	}
	if workers[0].NodeID != "w2" {
		t.Fatalf("a saturated pin is not preferred: %v", workers)
	}
	table["w1"] = LoadSignal{KVUsedPct: 20}
	r.mu.RLock()
	sat, _ = r.loadRank("w1")
	r.mu.RUnlock()
	if pin := r.prefixPick(ctx, "m"); pin != "" && !sat {
		workers = preferNode(workers, pin)
	}
	if workers[0].NodeID != "w1" {
		t.Fatalf("with headroom the pin wins: %v", workers)
	}
}
