package router

import (
	"context"
	"testing"

	"github.com/opod-io/opod-core/internal/engines"
)

// PLAN T26.22: the blocks a gang's head reports are scored for that gang, in
// the ranking a worker gets — a warm gang outweighs a little load, and never
// a full one.
func TestAGangHoldingThePromptWinsUntilItIsFull(t *testing.T) {
	r, _ := pinnedGangs(t)
	chain := []string{"b1", "b2", "b3", "b4"}
	r.SetPrefixBlocks(1, func(context.Context, engines.ChatRequest) []string { return chain },
		func(node string, hashes []string) int {
			if node == "n3" { // g1's head
				return len(hashes)
			}
			return 0
		})
	ctx := r.withPrefixBlocks(context.Background(), engines.ChatRequest{Model: "m"})
	r.mu.Lock()
	r.inflight["shard:m:g1"] = 1 // busier than g0, by less than its four warm blocks
	r.mu.Unlock()
	if _, key, _ := r.pick(ctx, "m"); key != "shard:m:g1" {
		t.Fatalf("the gang whose head holds the prompt lost to an idle cold one: %s", key)
	}
	r.mu.Lock()
	r.inflight["shard:m:g1"] = 2 // both slots in flight
	r.mu.Unlock()
	if _, key, _ := r.pick(ctx, "m"); key != "shard:m:g0" {
		t.Fatalf("a warm cache beat a full gang: %s", key)
	}
}
