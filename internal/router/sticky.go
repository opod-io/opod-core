package router

import (
	"context"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/store"
)

func (r *Router) stickyPick(ctx context.Context, model string) string {
	if r.stickyTTL <= 0 || model == "" || model == "auto" {
		return ""
	}
	userID := userIDFor(ctx)
	if userID == "" {
		return ""
	}
	key := userID + "|" + model
	r.mu.RLock()
	entry, ok := r.stickiness[key]
	r.mu.RUnlock()
	if !ok {
		metrics.ObserveStickyOutcome("miss")
		return ""
	}
	if time.Now().After(entry.ExpiresAt) {
		r.mu.Lock()
		// Re-check under write lock — another goroutine may have
		// already refreshed in the window.
		if e2, ok := r.stickiness[key]; ok && time.Now().After(e2.ExpiresAt) {
			delete(r.stickiness, key)
		}
		r.mu.Unlock()
		metrics.ObserveStickyOutcome("expired")
		return ""
	}
	return entry.NodeID
}

// rememberSticky refreshes the (user_id, model) → node pin after a
// successful engine call (called from Chat/Embed, not at pick time, so a
// picked-but-failed node isn't pinned). The next request for the same tuple
// within stickyTTL will land on the same node (assuming it's still healthy).
func (r *Router) rememberSticky(ctx context.Context, model, nodeID string) {
	if r.stickyTTL <= 0 || model == "" || model == "auto" || nodeID == "" {
		return
	}
	userID := userIDFor(ctx)
	if userID == "" {
		return
	}
	key := userID + "|" + model
	r.mu.Lock()
	r.stickiness[key] = stickyEntry{NodeID: nodeID, ExpiresAt: time.Now().Add(r.stickyTTL)}
	r.stickyInserts++
	// Opportunistic sweep: stickyPick only deletes the entry it looked
	// up, so pins for users who never return would accumulate forever.
	if r.stickyInserts%stickySweepEvery == 0 || len(r.stickiness) > stickySweepThreshold {
		now := time.Now()
		for k, e := range r.stickiness {
			if now.After(e.ExpiresAt) {
				delete(r.stickiness, k)
			}
		}
	}
	r.mu.Unlock()
}

// userIDFor returns the authenticated user id from ctx, or "" when no
// auth key is attached (dev mode with require_keys=false). Stickiness
// is disabled for the anonymous case.
func userIDFor(ctx context.Context) string {
	if k := auth.KeyFrom(ctx); k != nil {
		return k.UserID
	}
	return ""
}

// preferNode reorders `workers` so that `nodeID` is first if it's
// present. Other entries keep their existing relative order — this
// only nudges the sticky node to the front of the line, not the rest.
func preferNode(workers []store.Placement, nodeID string) []store.Placement {
	if len(workers) == 0 {
		return workers
	}
	for i, w := range workers {
		if w.NodeID == nodeID {
			if i == 0 {
				return workers
			}
			out := make([]store.Placement, 0, len(workers))
			out = append(out, workers[i])
			out = append(out, workers[:i]...)
			out = append(out, workers[i+1:]...)
			return out
		}
	}
	return workers
}

// CooldownUntil returns the time at which `nodeID` exits the penalty
// box, or a zero time if the node isn't currently in cooldown. Public
// so the admin API can decorate the Nodes list with a "🚫 cooldown"
// badge.
