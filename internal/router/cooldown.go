package router

import (
	"strings"
	"time"

	"github.com/opod-io/opod/internal/metrics"
)

// setAsideFor is how long a worker that gave a request NOTHING — it could not
// be dialled, its engine was down (engines.ErrWorkerEngineUnreachable), or its
// stream broke before the first event — is kept from new requests. One
// heartbeat interval: by then the worker's own report says whether its engine
// is gone (engineDownWhy, which then keeps it out for as long as that lasts),
// and a worker that was only restarting its engine is back in rotation.
const setAsideFor = 5 * time.Second

// setAside puts a worker that gave a request nothing in the penalty box for
// setAsideFor, whether or not the configured breaker (SetPlacementCooldown) is
// on: one such answer is a statement about the worker, not a strike. Without
// it, a worker whose engine had just died answered in milliseconds, looked the
// least loaded, and was picked for most new requests until its heartbeat said
// otherwise — each of them asked it first and then a sibling (design-partner
// cell, PLAN T8.18). The same map and gauge as the breaker; a longer cooldown
// already running is kept. A gang and the local engine are never benched here
// (recordOutcome's rule).
func (r *Router) setAside(nodeID string) {
	if nodeID == "" || nodeID == r.localNode || strings.HasPrefix(nodeID, "shard:") {
		return
	}
	until := time.Now().Add(setAsideFor)
	r.mu.Lock()
	if cur, ok := r.cooldowns[nodeID]; !ok || cur.Before(until) {
		r.cooldowns[nodeID] = until
	}
	n := len(r.cooldowns)
	r.mu.Unlock()
	metrics.SetRouterCooldownsActive(n)
	if r.log != nil {
		r.log.Info("router set a worker aside: it answered nothing", "node", nodeID, "for", setAsideFor)
	}
}

// inCooldown reads the penalty box, which the configured breaker and setAside
// both write.
func (r *Router) inCooldown(nodeID string) bool {
	r.mu.RLock()
	until, ok := r.cooldowns[nodeID]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	if time.Now().After(until) {
		// Cooldown expired — clean up so the gauge / debug view stays
		// honest. We don't reset failures here; the next successful
		// call does, so a still-flaky node re-enters cooldown on the
		// very next failure.
		r.mu.Lock()
		if until2, ok := r.cooldowns[nodeID]; ok && time.Now().After(until2) {
			delete(r.cooldowns, nodeID)
			metrics.SetRouterCooldownsActive(len(r.cooldowns))
			if r.log != nil {
				r.log.Info("router cooldown expired", "node", nodeID)
			}
		}
		r.mu.Unlock()
		return false
	}
	return true
}

// stickyPick returns the pinned node id for (user_id, model) when the
// pin is fresh, or "" when there's nothing to suggest. Bypassed for
// requests with no user_id and for the synthetic `auto` model id.
func (r *Router) CooldownUntil(nodeID string) time.Time {
	r.mu.RLock()
	until, ok := r.cooldowns[nodeID]
	r.mu.RUnlock()
	if !ok || time.Now().After(until) {
		return time.Time{}
	}
	return until
}

// recordOutcome notes the success/failure of an engine call from a
// remote worker. On `allowedFails` consecutive failures the node enters
// cooldown for `cooldownDur`. The first success after expiry resets
// the counter, so a node that flakes once doesn't shadow itself
// forever.
//
// nodeID == localNode is a no-op — cooldown only applies to remote
// workers. (A flaky local engine is a different operational problem;
// restart it.)
func (r *Router) recordOutcome(nodeID string, ok bool) {
	if nodeID == "" || nodeID == r.localNode || strings.HasPrefix(nodeID, "shard:") {
		return
	}
	if ok {
		// A worker in the box served a request — it was the only one left to
		// ask (takingRequests): it answers again, so it is back in rotation.
		r.mu.Lock()
		if _, boxed := r.cooldowns[nodeID]; boxed {
			delete(r.cooldowns, nodeID)
			metrics.SetRouterCooldownsActive(len(r.cooldowns))
		}
		r.mu.Unlock()
	}
	if r.placementCooldownDur <= 0 || r.placementAllowedFails <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ok {
		// Success: reset failure count if the node had any pending
		// strikes. Don't churn the metric — we only update the gauge
		// when entering/exiting cooldown.
		if r.failures[nodeID] > 0 {
			delete(r.failures, nodeID)
		}
		return
	}
	r.failures[nodeID]++
	if r.failures[nodeID] < r.placementAllowedFails {
		return
	}
	// Enter cooldown.
	r.cooldowns[nodeID] = time.Now().Add(r.placementCooldownDur)
	r.failures[nodeID] = 0
	metrics.SetRouterCooldownsActive(len(r.cooldowns))
	if r.log != nil {
		r.log.Warn("router placement cooldown",
			"node", nodeID,
			"allowed_fails", r.placementAllowedFails,
			"cooldown", r.placementCooldownDur,
		)
	}
}

// sameOrder returns true when two slices have identical contents in
// order. Cheap escape hatch so an entry whose typed list duplicates the
// generic list short-circuits to the existing behavior.
func sameOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// chainFor builds the candidate chain for `model`, accounting for any
// per-request overrides. The returned `source` is one of:
//
//   - "request" — Overrides.Fallbacks was non-empty; catalog fallbacks
//     are ignored for this request. Surfaced as a span attribute so
//     operators can tell at-a-trace who's bypassing catalog policy.
//   - "catalog" — fell through to the catalog-driven resolver (or just
//     the primary if no resolver / no `fallback:` entry).
//
// The returned `chains` carries the typed fallback lists; callers use
// this to swap the remainder of the chain after classifying the
// primary's failure. For source=="request" the returned chains is the
// zero value — typed selection only applies to catalog-driven routing.
