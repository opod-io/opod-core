package router

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

// TestStickyPick_DisabledWithoutTTL verifies the feature is opt-in.
func TestStickyPick_DisabledWithoutTTL(t *testing.T) {
	r := &Router{stickiness: map[string]stickyEntry{}}
	ctx := auth.WithTestKey(context.Background(), &store.APIKey{UserID: "alice"})
	if got := r.stickyPick(ctx, "qwen3-14b"); got != "" {
		t.Fatalf("ttl=0 should yield no sticky, got %q", got)
	}
}

// TestStickyPick_BypassesAnonymous — no auth context → no sticky.
func TestStickyPick_BypassesAnonymous(t *testing.T) {
	r := &Router{stickiness: map[string]stickyEntry{}}
	r.SetStickyTTL(time.Minute)
	if got := r.stickyPick(context.Background(), "qwen3-14b"); got != "" {
		t.Fatalf("anonymous ctx should yield no sticky, got %q", got)
	}
}

// TestStickyPick_BypassesAutoModel — auto resolves per-call, can't pin.
func TestStickyPick_BypassesAutoModel(t *testing.T) {
	r := &Router{stickiness: map[string]stickyEntry{}}
	r.SetStickyTTL(time.Minute)
	ctx := auth.WithTestKey(context.Background(), &store.APIKey{UserID: "alice"})
	r.rememberSticky(ctx, "auto", "worker-X")
	if got := r.stickyPick(ctx, "auto"); got != "" {
		t.Fatalf("auto should never produce a sticky, got %q", got)
	}
}

// TestStickyPick_RemembersAndReturns — happy path.
func TestStickyPick_RemembersAndReturns(t *testing.T) {
	r := &Router{stickiness: map[string]stickyEntry{}}
	r.SetStickyTTL(time.Minute)
	ctx := auth.WithTestKey(context.Background(), &store.APIKey{UserID: "alice"})
	r.rememberSticky(ctx, "qwen3-14b", "worker-X")

	if got := r.stickyPick(ctx, "qwen3-14b"); got != "worker-X" {
		t.Fatalf("got %q, want worker-X", got)
	}
	// Different user → no sticky.
	other := auth.WithTestKey(context.Background(), &store.APIKey{UserID: "bob"})
	if got := r.stickyPick(other, "qwen3-14b"); got != "" {
		t.Fatalf("bob should not inherit alice's pin, got %q", got)
	}
	// Different model → no sticky.
	if got := r.stickyPick(ctx, "claude-3-5-sonnet"); got != "" {
		t.Fatalf("model isolation broken, got %q", got)
	}
}

// TestStickyPick_ExpiresOnTTL — past the TTL, the entry is dropped.
func TestStickyPick_ExpiresOnTTL(t *testing.T) {
	r := &Router{stickiness: map[string]stickyEntry{}}
	r.SetStickyTTL(20 * time.Millisecond)
	ctx := auth.WithTestKey(context.Background(), &store.APIKey{UserID: "alice"})
	r.rememberSticky(ctx, "qwen3-14b", "worker-X")
	time.Sleep(40 * time.Millisecond)
	if got := r.stickyPick(ctx, "qwen3-14b"); got != "" {
		t.Fatalf("expired entry should not return a node, got %q", got)
	}
}

// TestPreferNode_MovesToFront — order matters; the helper must keep
// the rest of the slice's relative order to play nicely with the
// least-loaded sort.
func TestPreferNode_MovesToFront(t *testing.T) {
	workers := []store.Placement{
		{NodeID: "a"},
		{NodeID: "b"},
		{NodeID: "c"},
	}
	got := preferNode(workers, "c")
	if got[0].NodeID != "c" || got[1].NodeID != "a" || got[2].NodeID != "b" {
		t.Fatalf("preferNode(c): got %v", got)
	}
	// Already first — return as-is.
	got = preferNode(workers, "a")
	if got[0].NodeID != "a" {
		t.Fatalf("preferNode(a) should be idempotent, got %v", got)
	}
	// Not present — unchanged.
	got = preferNode(workers, "missing")
	if got[0].NodeID != "a" {
		t.Fatalf("preferNode(missing) should not reorder, got %v", got)
	}
}
