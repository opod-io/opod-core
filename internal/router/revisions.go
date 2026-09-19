package router

// Weighted routing between plan revisions (ROADMAP R15.17, feature
// "routing_weights").
//
// A canary that rolls one worker of N gives you 1/N of the traffic and no say
// in it: four workers means 25 %, and on a two-worker endpoint it means half.
// Weights decouple the two. Each worker registers the plan revision its process
// was started for; the picker chooses a revision GROUP by weight first, then
// the least-loaded worker inside that group — so load-aware routing, roles and
// prefix affinity all keep working underneath.
//
// Two rules make this safe rather than clever:
//
//   - a weight is a SHARE, not a percentage. The leader normalises whatever it
//     is given, so nothing has to add to 100 and a manager cannot create a
//     silent hole by sending 90 + 5;
//   - a group with NO LIVE WORKER is skipped and its share goes to the others.
//     A canary whose single pod is restarting must not black-hole its slice of
//     the traffic — that would turn a rolling pod into an outage for 10 % of
//     requests, which is exactly the failure a canary exists to avoid.
//
// With no weights configured — every endpoint that is not mid-canary — this
// does nothing at all and the picker behaves exactly as it did.

import (
	"context"
	"encoding/json"
	"math/rand"
	"sync"

	"github.com/opod-io/opod/internal/store"
)

// RevisionWeight is one revision's share of the traffic.
type RevisionWeight struct {
	Revision int `json:"revision"`
	Weight   int `json:"weight"`
}

type revisionRouting struct {
	mu      sync.RWMutex
	weights []RevisionWeight
}

// SetRevisionWeights applies the policy's split. An empty list turns it off.
func (r *Router) SetRevisionWeights(ws []RevisionWeight) {
	keep := make([]RevisionWeight, 0, len(ws))
	for _, w := range ws {
		if w.Weight > 0 && w.Revision > 0 {
			keep = append(keep, w)
		}
	}
	r.rev.mu.Lock()
	r.rev.weights = keep
	r.rev.mu.Unlock()
}

// revisionWeights is the configured split, copied out under the lock.
func (r *Router) revisionWeights() []RevisionWeight {
	r.rev.mu.RLock()
	defer r.rev.mu.RUnlock()
	return append([]RevisionWeight(nil), r.rev.weights...)
}

// revisionOf reads the plan revision a worker registered. Absent = 0, and
// every such worker forms one group together.
func revisionOf(n *store.Node) int {
	if n == nil || n.HardwareJSON == "" {
		return 0
	}
	var caps struct {
		PlanRevision int `json:"PlanRevision"`
	}
	if json.Unmarshal([]byte(n.HardwareJSON), &caps) != nil {
		return 0
	}
	return caps.PlanRevision
}

// chooseRevision picks a revision group by weight, considering only groups that
// actually have a worker. It returns 0 when there is nothing to choose between
// — no weights, one group, or no group with both a weight and a worker — and
// the caller then routes over every worker exactly as before.
func chooseRevision(ws []RevisionWeight, live map[int]int, roll func(int) int) int {
	total, candidates := 0, make([]RevisionWeight, 0, len(ws))
	for _, w := range ws {
		if live[w.Revision] == 0 {
			continue // no worker on this revision: its share goes to the rest
		}
		candidates = append(candidates, w)
		total += w.Weight
	}
	if len(candidates) < 2 || total <= 0 {
		return 0
	}
	n := roll(total)
	for _, w := range candidates {
		if n < w.Weight {
			return w.Revision
		}
		n -= w.Weight
	}
	return candidates[len(candidates)-1].Revision
}

// pickRevisionGroup narrows workers to one revision, when a split is
// configured and more than one revision is actually serving.
//
// A request is assigned to a group ONCE (nextworker.go): when it comes back for
// another worker of the same model it stays in its group while that group has a
// worker, instead of rolling the weights again.
func (r *Router) pickRevisionGroup(ctx context.Context, model string, workers []store.Placement, revision func(nodeID string) int) []store.Placement {
	ws := r.revisionWeights()
	if len(ws) == 0 {
		return workers
	}
	live := map[int]int{}
	for _, w := range workers {
		live[revision(w.NodeID)]++
	}
	chosen, again := assignedRevision(ctx, model)
	if !again || live[chosen] == 0 {
		if len(workers) < 2 {
			return workers
		}
		chosen = chooseRevision(ws, live, func(n int) int { return rand.Intn(n) }) //nolint:gosec // traffic splitting, not cryptography
		if chosen == 0 {
			return workers
		}
		assignRevision(ctx, model, chosen)
	}
	out := workers[:0:0]
	for _, w := range workers {
		if revision(w.NodeID) == chosen {
			out = append(out, w)
		}
	}
	if len(out) == 0 {
		return workers // cannot happen (live said otherwise), but never route to nothing
	}
	return out
}
