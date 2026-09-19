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
