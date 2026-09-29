package router

import (
	"context"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// held is a fake block index: node → the chain it holds from the start.
func heldBy(table map[string]int) BlockHolder {
	return func(node string, hashes []string) int {
		n := table[node]
		if n > len(hashes) {
			n = len(hashes)
		}
		return n
	}
}

func chain(n int) BlockResolver {
	return func(context.Context, engines.ChatRequest) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = string(rune('a' + i))
		}
		return out
	}
}

// The score a pick sorts by is the load score less the block credit, so a
// worker that holds the prefix outranks an idler one that does not — up to the
// weight, and never past saturation.
func TestBlockCreditJoinsTheLoadScore(t *testing.T) {
	table := map[string]LoadSignal{"warm": {QueueDepth: 2}, "cold": {}}
	r := newLoadRouter(table)
	r.SetLoadAware(1, 95, true)
	score := func(ctx context.Context, id string) (bool, float64, int) {
		r.mu.RLock()
		defer r.mu.RUnlock()
		sat, sc := r.loadRank(id)
		credit, blocks := r.blockCredit(ctx, id)
		return sat, sc - credit, blocks
	}
	req := engines.ChatRequest{Model: "m"}

	// Off: no resolver call, no credit, the load order stands.
	ctx := r.withPrefixBlocks(context.Background(), req)
	if _, w, b := score(ctx, "warm"); b != 0 || w != 2 {
		t.Fatalf("weight 0: warm scored %v with %d blocks, want 2 and none", w, b)
	}

	// On: warm holds 8 leading blocks at 0.5 each = 4 off a load of 2.
	r.SetPrefixBlocks(0.5, chain(8), heldBy(map[string]int{"warm": 8}))
	ctx = r.withPrefixBlocks(context.Background(), req)
	_, warm, wb := score(ctx, "warm")
	_, cold, cb := score(ctx, "cold")
	if wb != 8 || cb != 0 || !(warm < cold) {
		t.Fatalf("warm (%v, %d blocks) must outrank cold (%v, %d)", warm, wb, cold, cb)
	}

	// A little cache does not outweigh a lot of load: 1 block (0.5) against a queue of 2.
	r.SetPrefixBlocks(0.5, chain(8), heldBy(map[string]int{"warm": 1}))
	ctx = r.withPrefixBlocks(context.Background(), req)
	if _, warm, _ = score(ctx, "warm"); !(warm > cold) {
		t.Fatalf("one warm block (%v) must not beat an idle worker (%v)", warm, cold)
	}

	// Saturation is judged before any score, and a cache hit does not lift it.
	table["warm"] = LoadSignal{KVUsedPct: 99}
	r.SetPrefixBlocks(100, chain(8), heldBy(map[string]int{"warm": 8}))
	ctx = r.withPrefixBlocks(context.Background(), req)
	if sat, _, _ := score(ctx, "warm"); !sat {
		t.Fatal("a saturated worker stays saturated whatever it holds")
	}

	// A request the resolver cannot chain (no tokenizer, nothing indexed) scores as before.
	r.SetPrefixBlocks(0.5, func(context.Context, engines.ChatRequest) []string { return nil }, heldBy(map[string]int{"warm": 8}))
	ctx = r.withPrefixBlocks(context.Background(), req)
	if _, _, b := score(ctx, "warm"); b != 0 {
		t.Fatalf("no chain, no credit: got %d blocks", b)
	}
	if r.PrefixBlockWeight() != 0.5 {
		t.Fatalf("applied weight %v", r.PrefixBlockWeight())
	}
}
