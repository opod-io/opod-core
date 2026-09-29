package router

// Routing by the prefix cache a worker actually holds (feature
// "kv_block_events").
//
// The prefix pin in load.go remembers which worker LAST SERVED a prefix. It
// cannot know whether that worker's cache still holds the blocks — evicted
// under load, gone with a restart — and it cannot see a second worker that
// has the same prefix warm. Engines that publish cache events let the leader
// keep an index of the blocks each worker holds (leader/prefixindex.go); this
// file is where a request is scored against it.
//
//   score(worker) = load score − blockWeight × leading blocks of the request
//                   that the worker holds
//
// "Leading" because a prefix cache is a chain: block 3 is only a hit when
// blocks 1 and 2 were, so the count stops at the first block a worker lacks.
// The weight arrives in the policy snapshot like kvWeight does; 0 is off. A
// saturated worker still sorts behind every worker with headroom — a cache
// hit is not worth a queue — and when no candidate holds any of the request's
// blocks the prefix pin decides, as it did before.

import (
	"context"

	"github.com/opod-io/opod/internal/engines"
)

// BlockResolver answers the chain of block hashes for a request's prompt, or
// nil when it cannot (no tokenizer reachable, nothing indexed for the model).
type BlockResolver func(ctx context.Context, req engines.ChatRequest) []string

// BlockHolder answers how many LEADING hashes of the chain nodeID holds.
type BlockHolder func(nodeID string, hashes []string) int

// SetPrefixBlocks wires the block index into the picker. weight ≤ 0, or
// either function nil, turns block scoring off.
func (r *Router) SetPrefixBlocks(weight float64, resolve BlockResolver, held BlockHolder) {
	r.mu.Lock()
	if weight < 0 {
		weight = 0
	}
	r.blockWeight, r.blockResolve, r.blockHeld = weight, resolve, held
	r.mu.Unlock()
}

// PrefixBlockWeight reports the applied weight (for /admin/v1 and tests).
func (r *Router) PrefixBlockWeight() float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.blockWeight
}

type prefixBlocksCtx struct{}

// withPrefixBlocks resolves the request's block chain once, before any pick,
// and carries it for every pick this request makes (a retry on another worker
// scores against the same chain).
func (r *Router) withPrefixBlocks(ctx context.Context, req engines.ChatRequest) context.Context {
	r.mu.RLock()
	weight, resolve := r.blockWeight, r.blockResolve
	r.mu.RUnlock()
	if weight <= 0 || resolve == nil {
		return ctx
	}
	if hashes := resolve(ctx, req); len(hashes) > 0 {
		return context.WithValue(ctx, prefixBlocksCtx{}, hashes)
	}
	return ctx
}

func prefixBlocksFrom(ctx context.Context) []string {
	h, _ := ctx.Value(prefixBlocksCtx{}).([]string)
	return h
}

// blockCredit is what the block index takes off a worker's score for this
// request, and how many blocks that was. Caller holds r.mu (read).
func (r *Router) blockCredit(ctx context.Context, nodeID string) (credit float64, blocks int) {
	if r.blockWeight <= 0 || r.blockHeld == nil {
		return 0, 0
	}
	hashes := prefixBlocksFrom(ctx)
	if len(hashes) == 0 {
		return 0, 0
	}
	blocks = r.blockHeld(nodeID, hashes)
	return r.blockWeight * float64(blocks), blocks
}
