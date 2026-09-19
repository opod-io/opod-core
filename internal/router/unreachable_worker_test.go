package router

import (
	"context"
	"errors"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

func gone(node string) error {
	return &engines.UnreachableError{Engine: "vllm", Endpoint: "http://" + node, Err: errors.New("dial tcp: i/o timeout")}
}

// A worker that has just gone away (its pod was moved, its machine died) still
// holds a fresh placement row until its heartbeat ages out. A request picked
// for it could not connect — and was answered 502, although another worker was
// serving the same model: the walk went on to the next fallback MODEL, never to
// the next WORKER. Nothing was sent to the dead worker, so nothing is repeated
// by asking the other one. Seen as one failed chat per worker move.
func TestAnUnreachableWorkerCostsNoRequestWhileAnotherServesTheModel(t *testing.T) {
	r, _ := drainRouter(t,
		store.Node{ID: "gone", State: store.NodeStateReady},
		store.Node{ID: "alive", State: store.NodeStateReady})
	dead := &stubEngine{name: "gone", failFor: map[string]error{"m": gone("gone")}}
	live := &stubEngine{name: "alive"}
	r.remotes["gone"], r.remotes["alive"] = dead, live
	r.inflight["alive"] = 50 // load order alone picks the dead worker first

	stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m"})
	if err != nil {
		t.Fatalf("the request failed although worker `alive` serves the model: %v", err)
	}
	for range stream {
	}
	if len(dead.calls) != 1 || len(live.calls) != 1 {
		t.Fatalf("want one attempt on each worker, got gone=%v alive=%v", dead.calls, live.calls)
	}

	// Every worker unreachable: the first worker's error is what the caller hears,
	// and no worker is asked twice.
	dead.calls, live.calls = nil, nil
	live.failFor = map[string]error{"m": gone("alive")}
	if _, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m"}); !errors.Is(err, engines.ErrUnreachable) {
		t.Fatalf("all workers gone: want an unreachable error, got %v", err)
	}
	if len(dead.calls) != 1 || len(live.calls) != 1 {
		t.Fatalf("each worker is asked once, got gone=%v alive=%v", dead.calls, live.calls)
	}

	// An answer from the engine — here a refusal — is an answer: it is not retried elsewhere.
	dead.calls, live.calls = nil, nil
	dead.failFor = map[string]error{"m": errors.New("400 context length exceeded")}
	live.failFor = nil
	if _, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m"}); err == nil {
		t.Fatal("an engine's own refusal must reach the caller")
	}
	if len(live.calls) != 0 {
		t.Fatalf("a refusal was replayed on another worker: %v", live.calls)
	}
}

// The same rule for embeddings: one walk, one rule.
func TestAnUnreachableWorkerCostsNoEmbedding(t *testing.T) {
	r, _ := drainRouter(t,
		store.Node{ID: "gone", State: store.NodeStateReady},
		store.Node{ID: "alive", State: store.NodeStateReady})
	dead := &stubEngine{name: "gone", embedFail: map[string]error{"m": gone("gone")}}
	live := &stubEngine{name: "alive"}
	r.remotes["gone"], r.remotes["alive"] = dead, live
	r.inflight["alive"] = 50

	if _, err := r.Embed(context.Background(), engines.EmbedRequest{Model: "m", Inputs: []string{"x"}}); err != nil {
		t.Fatalf("the embedding failed although worker `alive` serves the model: %v", err)
	}
	if len(dead.calls) != 1 || len(live.calls) != 1 {
		t.Fatalf("want one attempt on each worker, got gone=%v alive=%v", dead.calls, live.calls)
	}
}

// Asking the next worker must not re-roll the traffic split. With a revision
// split configured, a request is first assigned to a revision group by weight.
// When the worker picked inside that group turned out to be gone, the re-pick
// rolled the weights AGAIN — every retry handed the canary another 20 % chance,
// so while a removed worker lingered in the old group a 20 % canary served 36 %
// (measured on a cluster: 69 of 194). The request stays in its group.
func TestNextWorkerStaysInTheRequestsRevisionGroup(t *testing.T) {
	r, _ := drainRouter(t,
		store.Node{ID: "old-gone", State: store.NodeStateReady, HardwareJSON: `{"PlanRevision":1}`},
		store.Node{ID: "old-alive", State: store.NodeStateReady, HardwareJSON: `{"PlanRevision":1}`},
		store.Node{ID: "canary", State: store.NodeStateReady, HardwareJSON: `{"PlanRevision":2}`})
	dead := &stubEngine{name: "old-gone", failFor: map[string]error{"m": gone("old-gone")}}
	alive, canary := &stubEngine{name: "old-alive"}, &stubEngine{name: "canary"}
	r.remotes["old-gone"], r.remotes["old-alive"], r.remotes["canary"] = dead, alive, canary
	r.SetRevisionWeights([]RevisionWeight{{Revision: 1, Weight: 80}, {Revision: 2, Weight: 20}})
	r.inflight["old-alive"] = 1000 // inside the old group the gone worker is always tried first

	const n = 3000
	for i := 0; i < n; i++ {
		stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m"})
		if err != nil {
			t.Fatalf("request %d failed: %v", i, err)
		}
		for range stream {
		}
		r.mu.Lock()
		r.inflight["old-alive"] = 1000 // keep the order; the stream's own decrement is not the point here
		r.mu.Unlock()
	}
	share := float64(len(canary.calls)) / n
	if share < 0.16 || share > 0.24 {
		t.Fatalf("the canary served %.1f %% of %d requests under a 20 %% split (a re-rolled retry gives 36 %%)", share*100, n)
	}
	if len(alive.calls)+len(canary.calls) != n {
		t.Fatalf("every request is served exactly once: old-alive=%d canary=%d", len(alive.calls), len(canary.calls))
	}
}
