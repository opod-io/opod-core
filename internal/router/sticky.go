package router

import (
	"context"
	"time"

	"github.com/opod-io/opod-core/internal/auth"
	"github.com/opod-io/opod-core/internal/metrics"
	"github.com/opod-io/opod-core/internal/store"
)

func (r *Router) stickyPick(ctx context.Context, model string) string {
	if r.StickyTTL() <= 0 || model == "" || model == "auto" {
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
	ttl := r.StickyTTL()
	if ttl <= 0 || model == "" || model == "auto" || nodeID == "" {
		return
	}
	userID := userIDFor(ctx)
	if userID == "" {
		return
	}
	key := userID + "|" + model
	r.mu.Lock()
	r.stickiness[key] = stickyEntry{NodeID: nodeID, ExpiresAt: time.Now().Add(ttl)}
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

// userIDFor returns whom a pin belongs to: the authenticated key's user
// id, else the key itself — a key with no user (every key a mounted auth
// snapshot creates, and `opod token create` without one) is still one
// caller, so it pins as one. "key:" keeps a key id apart from a user id.
// "" only when no key is attached (dev mode with require_keys=false):
// stickiness is disabled for the anonymous case.
func userIDFor(ctx context.Context) string {
	k := auth.KeyFrom(ctx)
	switch {
	case k == nil:
		return ""
	case k.UserID != "":
		return k.UserID
	case k.ID != "":
		return "key:" + k.ID
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
