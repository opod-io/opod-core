package router

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// A worker that has just gone away — its pod was moved, its machine died —
// keeps a fresh placement row until its heartbeat ages out. A GANG whose
// coordinator has gone is the same thing under its own key (skippedGang). A request picked
// for it cannot connect. The walk used to go on to the next fallback MODEL,
// so with no fallback configured the request failed although another worker
// was serving the same model: one lost request per worker move.
//
// An unreachable worker received nothing (engines.ErrUnreachable is a
// transport failure before any byte of an answer), so asking another worker
// repeats nothing. The failed node is set aside FOR THIS REQUEST ONLY — the
// cooldown counters (recordOutcome) are what keep later requests away — and
// the same model is picked again. Each worker can be set aside once, so the
// walk ends; when none is left the pick says so and the fallback chain goes
// on as before. An answer from an engine, a refusal included, is never
// replayed elsewhere.

// pickState is what one request remembers between its picks: the workers it
// found unreachable, and the revision group it was assigned to per model. The
// second matters as much as the first: a re-pick that rolled the revision
// weights AGAIN handed the canary another chance on every retry, so while a
// removed worker lingered in the old group a 20 % canary served 36 %. A request
// is assigned to a group once; asking the next worker stays inside it (unless
// the group has no worker left — then its share goes to the rest, as always).
type pickState struct {
	mu       sync.Mutex
	skipped  map[string]bool
	revision map[string]int  // model → the revision group this request was assigned to
	gangless map[string]bool // model → a gang of it was passed over as skipped on the last pick
}

type pickStateKey struct{}

// withPickState gives the request its state. Chat and Embed call it once, before the walk.
func withPickState(ctx context.Context) context.Context {
	if _, ok := ctx.Value(pickStateKey{}).(*pickState); ok {
		return ctx
	}
	return context.WithValue(ctx, pickStateKey{}, &pickState{skipped: map[string]bool{}, revision: map[string]int{}, gangless: map[string]bool{}})
}

func pickStateOf(ctx context.Context) *pickState {
	st, _ := ctx.Value(pickStateKey{}).(*pickState)
	return st
}

// withoutSkipped drops the workers this request already found unreachable.
func withoutSkipped(ctx context.Context, workers []store.Placement) []store.Placement {
	st := pickStateOf(ctx)
	if st == nil {
		return workers
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.skipped) == 0 {
		return workers
	}
	kept := make([]store.Placement, 0, len(workers))
	for _, w := range workers {
		if !st.skipped[w.NodeID] {
			kept = append(kept, w)
		}
	}
	return kept
}

// assignedRevision is the group this request was already assigned to for the
// model (ok=false: not yet); assignRevision records the first choice.
func assignedRevision(ctx context.Context, model string) (int, bool) {
	st := pickStateOf(ctx)
	if st == nil {
		return 0, false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	rev, ok := st.revision[model]
	return rev, ok
}

func assignRevision(ctx context.Context, model string, rev int) {
	if st := pickStateOf(ctx); st != nil {
		st.mu.Lock()
		st.revision[model] = rev
		st.mu.Unlock()
	}
}

// errNoWorkerLeft is the pick's answer when every worker that takes requests
// for the model was already found unreachable by this request.
var errNoWorkerLeft = errors.New("no reachable worker left")

func noWorkerLeft(model string) error {
	return fmt.Errorf("%w for %s: every worker serving it was unreachable for this request", errNoWorkerLeft, model)
}

// tryNextWorker says whether a failed start on nodeID should be followed by
// another pick of the SAME model, and returns the context that sets the node
// aside. Only a remote worker qualifies: the local engine and a shard
// coordinator have no sibling to ask.
func (r *Router) tryNextWorker(ctx context.Context, nodeID string, err error) (context.Context, bool) {
	st := pickStateOf(ctx)
	// A gang is set aside by its key (shard:<model>:<gang>), the id the pick
	// accounts it under: a coordinator that could not be dialled — its node
	// died, a part went and llama-server with it — is the same case as a
	// worker that could not be, and a sibling gang serving the same model is
	// the same answer. Before this the walk stopped at the first gang and one
	// request in flight failed per lost part (design-partner cell, T8.7:
	// 59 of 60).
	if st == nil || !errors.Is(err, engines.ErrUnreachable) || nodeID == "" || nodeID == r.localNode {
		return ctx, false
	}
	st.mu.Lock()
	st.skipped[nodeID] = true
	st.mu.Unlock()
	return ctx, true
}

// skippedGang says whether this request already found the gang unreachable.
func skippedGang(ctx context.Context, key string) bool {
	st := pickStateOf(ctx)
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.skipped[key]
}

// noteSkippedGang records that a routable gang of the model was passed over
// because this request had set it aside; skippedEveryGang reads it after a
// pick that found no gang, so "every gang unreachable" is told apart from
// "the model has no gang".
func noteSkippedGang(ctx context.Context, model string) {
	if st := pickStateOf(ctx); st != nil {
		st.mu.Lock()
		st.gangless[model] = true
		st.mu.Unlock()
	}
}

func skippedEveryGang(ctx context.Context, model string) bool {
	st := pickStateOf(ctx)
	if st == nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.gangless[model]
}
