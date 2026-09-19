package router

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// A worker that has just gone away — its pod was moved, its machine died —
// keeps a fresh placement row until its heartbeat ages out. A request picked
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

type skippedNodesKey struct{}

// skipNode returns ctx with node set aside for the rest of this request.
func skipNode(ctx context.Context, node string) context.Context {
	prev, _ := ctx.Value(skippedNodesKey{}).(map[string]bool)
	next := make(map[string]bool, len(prev)+1)
	for k := range prev {
		next[k] = true
	}
	next[node] = true
	return context.WithValue(ctx, skippedNodesKey{}, next)
}

// withoutSkipped drops the workers this request already found unreachable.
func withoutSkipped(ctx context.Context, workers []store.Placement) []store.Placement {
	skipped, _ := ctx.Value(skippedNodesKey{}).(map[string]bool)
	if len(skipped) == 0 {
		return workers
	}
	kept := make([]store.Placement, 0, len(workers))
	for _, w := range workers {
		if !skipped[w.NodeID] {
			kept = append(kept, w)
		}
	}
	return kept
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
	if !errors.Is(err, engines.ErrUnreachable) || nodeID == "" || nodeID == r.localNode || strings.HasPrefix(nodeID, "shard:") {
		return ctx, false
	}
	return skipNode(ctx, nodeID), true
}
