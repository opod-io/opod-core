package router

// The CPU-tier view of the prefix score (feature "kv_cpu_tier_hits", PLAN
// T24.56): under routing.prefixBlockTier cpu-lower, a leading block a worker
// holds in its own host-memory tier beyond its card's is credited at
// cpuBlockShare of a card block; by default the card alone counts.

import (
	"context"
	"testing"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/store"
)

// cpuPick sets up two workers of model "m": "warm" holds the request's eight
// leading blocks in its CPU tier only and has one request in flight, "cold"
// holds nothing and is idle.
func cpuPick(t *testing.T, cpuLower bool) string {
	t.Helper()
	r, _ := drainRouter(t,
		store.Node{ID: "warm", State: store.NodeStateReady},
		store.Node{ID: "cold", State: store.NodeStateReady})
	r.SetPrefixBlocks(0.5, chain(8), heldBy(map[string]int{}))
	if cpuLower {
		r.SetPrefixBlockCPU(heldBy(map[string]int{"warm": 8}))
	}
	r.inflight["warm"], r.inflight["cold"] = 1, 0
	ctx := r.withPrefixBlocks(context.Background(), engines.ChatRequest{Model: "m"})
	_, node, err := r.pick(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// The row's rule: under cpu-lower the worker whose CPU tier holds the prompt
// outranks an idle cold one (1 − 8×0.5×0.8 = −2.2 against 0); by default the
// CPU tier is not counted and the idle worker wins, as before.
func TestCPULowerCreditsTheCPUTier(t *testing.T) {
	if got := cpuPick(t, true); got != "warm" {
		t.Fatalf("cpu-lower: picked %q, want the worker whose CPU tier holds the prompt", got)
	}
	if got := cpuPick(t, false); got != "cold" {
		t.Fatalf("default (gpu): picked %q, want the idle worker — the CPU tier must not count", got)
	}
}

// A card block outranks the same block in a CPU tier at equal load, and a CPU
// block is credited only beyond the card's own leading blocks.
func TestBlockCreditsCountsTheCPUTierBeyondTheCard(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{})
	r.SetPrefixBlocks(1, chain(8), heldBy(map[string]int{"card": 8, "both": 3, "cpu": 0}))
	r.SetPrefixBlockCPU(heldBy(map[string]int{"card": 8, "both": 8, "cpu": 8}))
	ctx := r.withPrefixBlocks(context.Background(), engines.ChatRequest{Model: "m"})
	r.mu.RLock()
	hits := r.blockCredits(ctx, []string{"card", "both", "cpu"}, func(string) bool { return false })
	r.mu.RUnlock()
	want := map[string]blockHit{
		"card": {credit: 8, blocks: 8, kind: hitLocal},
		"both": {credit: 3 + 5*cpuBlockShare, blocks: 3, cpu: 5, kind: hitLocal},
		"cpu":  {credit: 8 * cpuBlockShare, cpu: 8, kind: hitCPU},
	}
	for id, w := range want {
		if got := hits[id]; got != w {
			t.Errorf("%s: %+v, want %+v", id, got, w)
		}
	}
}
