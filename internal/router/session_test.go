package router

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/auth"
	"github.com/opod-io/opod-core/internal/store"
)

// ADR-109 §2: two conversations under one key each keep their own worker; a
// request with no session keeps the key's pin, as before.
func TestTwoSessionsUnderOneKeyPinApart(t *testing.T) {
	r := New(nil, nil)
	key := auth.WithTestKey(context.Background(), &store.APIKey{ID: "k1"})
	a := WithSession(key, "conv-a", time.Minute)
	b := WithSession(key, "conv-b", time.Minute)
	r.rememberSticky(a, "m", "w1")
	r.rememberSticky(b, "m", "w2")
	if got := r.stickyPick(a, "m"); got != "w1" {
		t.Fatalf("conversation a pinned to %q, want w1", got)
	}
	if got := r.stickyPick(b, "m"); got != "w2" {
		t.Fatalf("conversation b pinned to %q, want w2", got)
	}
	if got := r.stickyPick(key, "m"); got != "" {
		t.Fatalf("a request with no session read a session's pin: %q", got)
	}
	if WithSession(key, string(make([]byte, MaxSessionIDBytes+1)), time.Minute) != key {
		t.Fatal("an over-long session id must attach nothing")
	}
}

// A verified route names the worker first, and a pin never beats load: a
// saturated or full worker is not preferred.
func TestAPinNeverBeatsLoad(t *testing.T) {
	r := New(nil, nil)
	ctx := WithRoutedNode(context.Background(), "w1")
	if got := r.stickyPick(ctx, "m"); got != "w1" {
		t.Fatalf("routed node: %q", got)
	}
	if !r.pinHonoured("w1") {
		t.Fatal("an idle worker with no sample is a fine pin")
	}
	r.SetLoadAware(1, 90, false)
	r.SetLoadSource(func(string) (LoadSignal, bool) { return LoadSignal{KVUsedPct: 95}, true })
	if r.pinHonoured("w1") {
		t.Fatal("a pin was honoured on a worker at 95 % KV (saturation 90)")
	}
	r.SetLoadSource(func(string) (LoadSignal, bool) { return LoadSignal{KVUsedPct: 10}, true })
	r.SetSlotSource(func(string) (int, bool) { return 1, true })
	r.mu.Lock()
	r.inflight["w1"] = 1
	r.mu.Unlock()
	if r.pinHonoured("w1") {
		t.Fatal("a pin was honoured on a worker with no free slot")
	}
}
