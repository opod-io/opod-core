package router

// T28.8: a gang is ranked by the order a worker is. Its session pin, prefix
// pin and route token were written after every answer and never read, so a
// later turn of a conversation went to the least loaded gang and recomputed
// its prompt.

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/store"
)

// pinnedGangs is a router with two ready gangs of m, each served by a stub, two
// slots per gang, prefix affinity on and a sticky TTL to keep pins by.
func pinnedGangs(t *testing.T) (*Router, map[string]*stubEngine) {
	t.Helper()
	r, _ := gangRouter(t, "m", map[string][2]string{"g0": {"n1", "n2"}, "g1": {"n3", "n4"}},
		store.Node{ID: "n1"}, store.Node{ID: "n2"}, store.Node{ID: "n3"}, store.Node{ID: "n4"})
	stubs := map[string]*stubEngine{"shard:m:g0": {name: "g0"}, "shard:m:g1": {name: "g1"}}
	for k, s := range stubs {
		r.remotes[k] = s
	}
	r.SetStickyTTL(time.Minute)
	r.SetLoadAware(0, 0, true)
	r.SetSlotSource(func(string) (int, bool) { return 2, true })
	return r, stubs
}

// chatOn serves one request and names the gang it went to.
func chatOn(t *testing.T, r *Router, stubs map[string]*stubEngine, ctx context.Context, req engines.ChatRequest) string {
	t.Helper()
	for _, s := range stubs {
		s.calls = nil
	}
	stream, err := r.Chat(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}
	for k, s := range stubs {
		if len(s.calls) > 0 {
			return k
		}
	}
	t.Fatal("no gang was asked")
	return ""
}

func TestAConversationStaysOnItsGangUntilThatGangIsFull(t *testing.T) {
	r, stubs := pinnedGangs(t)
	turn := engines.ChatRequest{Model: "m", System: "a long shared system prompt", Messages: []engines.Message{{Role: "user", Content: "hello"}}}
	conv := WithSession(context.Background(), "conv-1", 0)

	first := chatOn(t, r, stubs, conv, turn)
	other := "shard:m:g1"
	if first == other {
		other = "shard:m:g0"
	}

	// The first gang is now busier than the other: load alone sends the next
	// request away — a request of another conversation and another prompt does.
	r.mu.Lock()
	r.inflight[first] = 1
	r.mu.Unlock()
	stranger := WithSession(context.Background(), "conv-2", 0)
	if got := chatOn(t, r, stubs, stranger, engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: "unrelated"}}}); got != other {
		t.Fatalf("control: with no pin the idle gang must win, got %s", got)
	}

	// The conversation's next turn keeps its gang while it has a free slot.
	if got := chatOn(t, r, stubs, conv, turn); got != first {
		t.Fatalf("a later turn left its gang %s for %s although that gang had a free slot", first, got)
	}
	// The same prompt under another conversation follows the prefix pin.
	if got := chatOn(t, r, stubs, WithSession(context.Background(), "conv-3", 0), turn); got != first {
		t.Fatalf("the prefix pin was not read for a gang: got %s, want %s", got, first)
	}

	// Its gang is full: the pin never lands a request on it.
	r.mu.Lock()
	r.inflight[first] = 2
	r.mu.Unlock()
	if got := chatOn(t, r, stubs, conv, turn); got != other {
		t.Fatalf("a pin put a request on a gang with no free slot (%s)", got)
	}
}

// A verified route token naming a gang (the leader's verifyRoute returns its
// key) routes there first, and only while that gang has headroom.
func TestARouteTokenNamingAGangRoutesThere(t *testing.T) {
	r, _ := pinnedGangs(t)
	r.mu.Lock()
	r.inflight["shard:m:g1"] = 1 // load alone picks g0
	r.mu.Unlock()
	ctx := WithRoutedNode(WithSession(context.Background(), "conv", 0), "shard:m:g1")
	if _, key, _ := r.pick(ctx, "m"); key != "shard:m:g1" {
		t.Fatalf("the token's gang was not preferred: got %s", key)
	}
	r.mu.Lock()
	r.inflight["shard:m:g1"] = 2
	r.mu.Unlock()
	if _, key, _ := r.pick(ctx, "m"); key != "shard:m:g0" {
		t.Fatalf("the token put a request on a full gang: got %s", key)
	}
}

// The pin guard reads the gang's own rank — its head's sample — never the gang
// key looked up as if it were a worker: a gang whose head is at the KV
// saturation line is not pinned to.
func TestAGangPinNeverLandsOnASaturatedHead(t *testing.T) {
	r, _ := pinnedGangs(t)
	r.SetLoadAware(0, 90, true)
	r.SetLoadSource(func(node string) (LoadSignal, bool) {
		if node == "n1" { // g0's head
			return LoadSignal{KVUsedPct: 95}, true
		}
		return LoadSignal{}, true
	})
	ctx := WithRoutedNode(context.Background(), "shard:m:g0")
	if _, key, _ := r.pick(ctx, "m"); key != "shard:m:g1" {
		t.Fatalf("a pin landed on a gang whose head is at 95 %% KV: got %s", key)
	}
}
