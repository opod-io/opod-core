package router

// One ranking for every place a request can go (PLAN T28.8, design (B)).
//
// A request goes to a worker, or to a gang (one copy of a model split across
// several workers, served through its head). Both are candidates of the same
// pick, and both pass the same order:
//
//  1. load: in flight + the engine's queue + what was sent after its sample +
//     kvWeight × KV use, minus the block credit (the leading blocks of this
//     prompt its engine holds, prefixblocks.go). A saturated candidate — its
//     slots all in flight, or its KV at the saturation line — sorts after
//     every candidate with headroom. Equal candidates keep their order, or
//     take turns (gangs, so a second gang carries requests too).
//  2. the prefix pin (load.go), read only when no candidate holds any of the
//     prompt's blocks: it is a guess, and the block index is not.
//  3. the conversation's pin — a verified route token, else the session or
//     sticky pin (sticky.go, session.go) — applied last, so it wins.
//
// The invariant: a pin never lands a request on a saturated candidate
// (ADR-109 §4). It is judged from the candidate's own rank — for a gang that
// is its head's sample and the gang's slots, never a lookup of the gang key
// as if it were a worker.
//
// What a candidate is counted under (key) and whose signals it is read by
// (signal, blocks) are separate, because they differ for a gang: its requests
// are counted under the gang key, and its load and its blocks are those of
// its head worker. For a worker all three are its node id.

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/opod-io/opod-core/internal/metrics"
)

// candidate is one place the pick may send a request.
type candidate struct {
	key    string // in flight, slots, sends and pins are counted under it: a node id, or gangKey
	signal string // the node whose pulled load sample is read; "" = none (scored on in-flight alone)
	blocks string // the node whose KV block index is read; "" = none
	peer   bool   // its engine keeps a peer KV tier (prefixblocks.go)
}

// workerCandidate is a worker as a candidate: every signal is its own.
func workerCandidate(nodeID string, peer bool) candidate {
	return candidate{key: nodeID, signal: nodeID, blocks: nodeID, peer: peer}
}

// candRank is one candidate's place in the order: saturated after every
// unsaturated candidate, then by score, lowest first. Caller holds r.mu (read).
func (r *Router) candRank(c candidate) (saturated bool, score float64) {
	score = float64(r.inflight[c.key])
	if r.slotsFull(c.key) {
		return true, score // every slot busy: a request here waits in the engine's queue (ADR-091)
	}
	if r.loadSource == nil || c.signal == "" {
		return false, score
	}
	sig, ok := r.loadSource(c.signal)
	if !ok {
		return false, score
	}
	score += float64(sig.QueueDepth)
	if !sig.SampledAt.IsZero() {
		// Sent after the sample: not in its queue figure yet (ADR-109), so a
		// burst between two pulls spreads instead of herding on one candidate.
		score += float64(r.sentSinceLocked(c.key, sig.SampledAt))
	}
	if r.kvWeight > 0 {
		score += r.kvWeight * sig.KVUsedPct / 100
	}
	saturated = r.kvSaturationPct > 0 && sig.KVUsedPct >= float64(r.kvSaturationPct)
	return saturated, score
}

// candidateRank is a candidate's computed place, kept for the pin guard.
type candidateRank struct {
	saturated bool
	score     float64
}

// rankCandidates orders cands for this request (steps 1–3 above) and returns
// them, the first being the pick, with the conversation pin it consulted (""
// = none). turn, when not nil, makes the candidates tied for first take turns
// rather than the first in input order winning. cands must not be empty.
// Takes r.mu itself; the caller must not hold it.
func (r *Router) rankCandidates(ctx context.Context, model string, cands []candidate, turn *atomic.Uint64) ([]candidate, string) {
	r.mu.RLock()
	rank := make(map[string]candidateRank, len(cands))
	byBlocks := make(map[string]bool, len(cands)) // blocks node → its peer tier
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		if c.blocks != "" {
			ids = append(ids, c.blocks)
			byBlocks[c.blocks] = c.peer
		}
	}
	// Blocks the candidates' engines actually hold for this prompt, their own
	// and — with a peer tier — a sibling's: a warm cache outweighs a little
	// load, a peer-warm one less of it.
	hits := r.blockCredits(ctx, ids, func(id string) bool { return byBlocks[id] })
	if r.log != nil && hits != nil {
		// What the block index credited each candidate with (debug: one line
		// per pick that had a chain), so an operator can see why a candidate
		// that holds the prompt did or did not win.
		per := make([]string, 0, len(cands))
		for _, c := range cands {
			h := hits[c.blocks]
			per = append(per, fmt.Sprintf("%s:card=%d,cpu=%d,peer=%d,credit=%.2f", c.key, h.blocks, h.cpu, h.peer, h.credit))
		}
		r.log.Debug("prefix block credits", "model", model, "chain", len(prefixBlocksFrom(ctx)), "candidates", strings.Join(per, " "))
	}
	warm := false // some candidate holds blocks of this prompt, or can fetch them from a peer
	for _, c := range cands {
		sat, sc := r.candRank(c)
		var hit blockHit
		if c.blocks != "" {
			hit = hits[c.blocks]
		}
		if hit.kind != "" {
			warm = true
		}
		rank[c.key] = candidateRank{sat, sc - hit.credit}
	}
	r.mu.RUnlock()

	order := append([]candidate(nil), cands...)
	sort.SliceStable(order, func(i, j int) bool {
		a, b := rank[order[i].key], rank[order[j].key]
		if a.saturated != b.saturated {
			return !a.saturated
		}
		return a.score < b.score
	})
	if turn != nil {
		tied := 1
		for tied < len(order) && rank[order[tied].key] == rank[order[0].key] {
			tied++
		}
		if tied > 1 {
			order = preferKey(order, order[int(turn.Add(1)-1)%tied].key)
		}
	}

	// A pin is honoured only on a candidate of this pick with headroom.
	usable := func(key string) bool {
		rk, ok := rank[key]
		return ok && !rk.saturated
	}
	// 2. Prefix pin: the candidate that last served the longest part of this
	// prompt's lead likely still holds it. Consulted only when nobody reports
	// holding anything — the score above already decided from what engines hold.
	if !warm {
		if pin := r.prefixPick(ctx, model, usable); pin != "" {
			order = preferKey(order, pin)
		}
	}
	// 3. The conversation's pin, last: cache locality outweighs a small load
	// difference, never a queue.
	sticky := r.stickyPick(ctx, model)
	if sticky != "" && usable(sticky) {
		order = preferKey(order, sticky)
	}
	return order, sticky
}

// notePinOutcome records what the conversation's pin did on this pick, once:
// stickyPick already counted "miss" and "expired" when there was no live pin.
func notePinOutcome(ctx context.Context, sticky, chosen string) {
	switch {
	case sticky == "":
		notePin(ctx, "")
	case chosen == sticky:
		metrics.ObserveStickyOutcome("hit")
		notePin(ctx, PinHit)
	default:
		metrics.ObserveStickyOutcome("miss")
		notePin(ctx, PinMoved)
	}
}

// preferKey moves the candidate keyed key to the front; the others keep their
// relative order. Unchanged when key is not among them.
func preferKey(cands []candidate, key string) []candidate {
	for i, c := range cands {
		if c.key != key {
			continue
		}
		if i == 0 {
			return cands
		}
		out := make([]candidate, 0, len(cands))
		out = append(out, c)
		out = append(out, cands[:i]...)
		return append(out, cands[i+1:]...)
	}
	return cands
}
