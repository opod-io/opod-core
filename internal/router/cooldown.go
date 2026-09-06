package router

import (
	"strings"
	"time"

	"github.com/opod-io/opod/internal/metrics"
)

func (r *Router) inCooldown(nodeID string) bool {
	if r.placementCooldownDur <= 0 {
		return false
	}
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
	if r.placementCooldownDur <= 0 {
		return time.Time{}
	}
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
	if r.placementCooldownDur <= 0 || r.placementAllowedFails <= 0 {
		return
	}
	if nodeID == "" || nodeID == r.localNode || strings.HasPrefix(nodeID, "shard:") {
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
