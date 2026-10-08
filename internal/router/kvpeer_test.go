package router

// The peer view of the prefix score (feature "kv_peer_hits", ADR-089): a
// worker whose engine shares its KV cache tier with its siblings can fetch a
// prefix a sibling holds, so a hit is local, peer or none — in that order.

import (
	"context"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

const peerCaps = `{"KVTier":"peer"}`

// peerPick sets up two workers of model "m": "busy" holds the request's eight
// leading blocks with three requests in flight, "free" holds none and is idle.
func peerPick(t *testing.T, busyCaps, freeCaps string, busyLoad, freeLoad int) string {
	t.Helper()
	r, _ := drainRouter(t,
		store.Node{ID: "busy", State: store.NodeStateReady, HardwareJSON: busyCaps},
		store.Node{ID: "free", State: store.NodeStateReady, HardwareJSON: freeCaps})
	r.SetPrefixBlocks(0.5, chain(8), heldBy(map[string]int{"busy": 8}))
	r.inflight["busy"], r.inflight["free"] = busyLoad, freeLoad
	ctx := r.withPrefixBlocks(context.Background(), engines.ChatRequest{Model: "m"})
	_, node, err := r.pick(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	return node
}

// The row's proof: a free worker with a peer hit beats a busy worker with a
// local hit, because a peer hit lets load outweigh affinity. Busy scores
// 3 − 8×0.5 = −1; free scores 0 − 8×0.5×peerBlockShare = −2.
func TestAFreeWorkerWithAPeerHitBeatsABusyWorkerWithALocalOne(t *testing.T) {
	if got := peerPick(t, peerCaps, peerCaps, 3, 0); got != "free" {
		t.Fatalf("both workers share a peer tier: picked %q, want the free one that can fetch the prefix", got)
	}
}

// With no peer tier the score is today's: the busy holder of the prefix wins
// over the cold idle worker (−1 against 0). A peer tier on ONE side changes
// nothing — a worker can fetch only from a sibling that shares its tier.
func TestWithoutAPeerTierOnBothSidesTheLocalHolderWinsAsBefore(t *testing.T) {
	for _, tc := range []struct{ name, busy, free string }{
		{"no tier", "", ""},
		{"cpu tiers", `{"KVTier":"cpu"}`, `{"KVTier":"cpu"}`},
		{"only the free worker", "", peerCaps},
		{"only the holder", peerCaps, ""},
	} {
		if got := peerPick(t, tc.busy, tc.free, 3, 0); got != "busy" {
			t.Fatalf("%s: picked %q, want busy (the local holder, as before the peer tier)", tc.name, got)
		}
	}
}

// Local beats peer at equal load: the holder's own blocks are worth more than
// the same blocks fetched across the network.
func TestALocalHitBeatsAPeerHitAtEqualLoad(t *testing.T) {
	if got := peerPick(t, peerCaps, peerCaps, 1, 1); got != "busy" {
		t.Fatalf("equal load: picked %q, want the local holder", got)
	}
}

// The scorer's kinds: local where the worker holds the chain's head, peer for
// the further blocks the best OTHER peer sibling holds, none otherwise; a
// worker's own blocks are never counted again as a peer hit.
func TestBlockCreditsNamesTheKindOfHit(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{})
	r.SetPrefixBlocks(1, chain(8), heldBy(map[string]int{"long": 8, "short": 3, "cold": 0, "loner": 5}))
	ctx := r.withPrefixBlocks(context.Background(), engines.ChatRequest{Model: "m"})
	peers := map[string]bool{"long": true, "short": true, "cold": true}
	r.mu.RLock()
	hits := r.blockCredits(ctx, []string{"long", "short", "cold", "loner"}, func(id string) bool { return peers[id] })
	r.mu.RUnlock()

	want := map[string]blockHit{
		// its best sibling is "short" (3), which it already holds: no peer blocks
		"long": {credit: 8, blocks: 8, kind: hitLocal},
		// 3 of its own + the 5 further blocks "long" holds, at half
		"short": {credit: 3 + 5*peerBlockShare, blocks: 3, peer: 5, kind: hitLocal},
		// nothing of its own, all 8 from "long"
		"cold": {credit: 8 * peerBlockShare, peer: 8, kind: hitPeer},
		// no peer tier: its own 5 only, and "long" is not its sibling
		"loner": {credit: 5, blocks: 5, kind: hitLocal},
	}
	for id, w := range want {
		if got := hits[id]; got != w {
			t.Errorf("%s: got %+v, want %+v", id, got, w)
		}
	}

	// Off, or no chain: nil, so every lookup is the zero hit.
	r.SetPrefixBlocks(0, chain(8), heldBy(nil))
	ctx = r.withPrefixBlocks(context.Background(), engines.ChatRequest{Model: "m"})
	r.mu.RLock()
	defer r.mu.RUnlock()
	if hits := r.blockCredits(ctx, []string{"long"}, func(string) bool { return true }); hits != nil {
		t.Fatalf("block scoring off: %v, want nil", hits)
	}
}
