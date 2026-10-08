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
//
// A worker whose engine keeps a PEER tier (feature "kv_peer_hits", ADR-089)
// can fetch blocks a sibling's tier holds instead of computing them, so a hit
// has three kinds, scored in that order:
//
//   local  the worker's own leading blocks          blockWeight each
//   peer   the further leading blocks the best
//          sibling with a peer tier holds           blockWeight × peerBlockShare each
//   none                                            nothing
//
// A peer hit is real but slower than a local one — the blocks still cross
// the network — so it is credited at a fraction, which is what lets load
// outweigh affinity: a busy worker holding the prefix no longer beats an idle
// sibling that can fetch it. Only workers that BOTH state a peer tier share
// blocks; with no peer tier among the candidates the score is exactly the
// local one, block for block.

import (
	"context"
	"encoding/json"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// KVTierPeer is the Capabilities.KVTier a worker registers when its engine's
// KV cache tier is shared with its siblings (OPOD_KV_TIER=peer).
const KVTierPeer = "peer"

// peerBlockShare is what a block fetched from a sibling's tier is worth
// against one the worker holds itself. A fixed half until a measured TTFT with
// the peer tier on and off says better: below 1 so a local hit outranks a
// peer hit at equal load, above 0 so a peer hit outranks a cold worker.
const peerBlockShare = 0.5

// Hit kinds (blockHit.kind).
const (
	hitLocal = "local"
	hitPeer  = "peer"
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

// blockCredit is what the block index takes off one worker's score for this
// request, and how many blocks that was, with no sibling to fetch from: the
// local half of blockCredits. Caller holds r.mu (read).
func (r *Router) blockCredit(ctx context.Context, nodeID string) (credit float64, blocks int) {
	h := r.blockCredits(ctx, []string{nodeID}, func(string) bool { return false })[nodeID]
	return h.credit, h.blocks
}

// blockHit is one candidate's credit against the request's block chain, and
// which kind of hit earned it ("" = none).
type blockHit struct {
	credit float64
	blocks int // the candidate's own leading blocks
	peer   int // further leading blocks a sibling's peer tier holds for it
	kind   string
}

// blockCredits scores every candidate against the request's chain, with the
// peer view: a candidate that states a peer tier is credited, beyond its own
// blocks, for the further leading blocks the best OTHER peer candidate holds.
// Caller holds r.mu (read). nil when block scoring is off or the request has
// no chain — every lookup then reads the zero hit, which is today's score.
func (r *Router) blockCredits(ctx context.Context, ids []string, peer func(id string) bool) map[string]blockHit {
	if r.blockWeight <= 0 || r.blockHeld == nil {
		return nil
	}
	hashes := prefixBlocksFrom(ctx)
	if len(hashes) == 0 {
		return nil
	}
	out := make(map[string]blockHit, len(ids))
	// The two longest chains among peer candidates: a candidate's best
	// sibling is the first unless that is itself.
	var best, second int
	bestID := ""
	for _, id := range ids {
		n := r.blockHeld(id, hashes)
		out[id] = blockHit{blocks: n}
		if !peer(id) {
			continue
		}
		switch {
		case n > best:
			second, best, bestID = best, n, id
		case n > second:
			second = n
		}
	}
	for id, h := range out {
		if peer(id) {
			sib := best
			if id == bestID {
				sib = second
			}
			h.peer = max(0, sib-h.blocks)
		}
		h.credit = r.blockWeight * (float64(h.blocks) + peerBlockShare*float64(h.peer))
		switch {
		case h.blocks > 0:
			h.kind = hitLocal
		case h.peer > 0:
			h.kind = hitPeer
		}
		out[id] = h
	}
	return out
}

// kvTierOf reads the KV cache tier a worker registered (capabilities JSON,
// field KVTier); "" = VRAM only, or a worker that predates the field.
func kvTierOf(n *store.Node) string {
	if n == nil || n.HardwareJSON == "" {
		return ""
	}
	var caps struct {
		KVTier string `json:"KVTier"`
	}
	if json.Unmarshal([]byte(n.HardwareJSON), &caps) != nil {
		return ""
	}
	return caps.KVTier
}
