package router

// A worker that gave a request nothing — here, its engine was down — is kept
// from NEW requests for a moment, with the configured breaker OFF (its
// default). Before, the request itself went to a sibling but the next request
// picked the dead worker again: it answered in milliseconds and so looked the
// least loaded (design-partner cell, PLAN T8.18: for ~2 s after an engine
// kill, every other request failed in 2–3 ms).

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

func TestAWorkerWhoseEngineIsDownIsSetAsideForNewRequests(t *testing.T) {
	down := newPlainWorker(t, http.StatusBadGateway, "application/json", workerEngineDown)
	healthy := newSSEWorker(t, delta("hello from w1"), "data: [DONE]\n\n")
	r := workersOf(t, engines.MustNew("vllm", down.srv.URL, ""), healthy) // no SetPlacementCooldown: the default

	for i := 0; i < 5; i++ {
		stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true})
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		if text, evErr := drainAnswer(t, stream); evErr != nil || text != "hello from w1" {
			t.Fatalf("request %d: want w1's answer, got %q (err %v)", i, text, evErr)
		}
	}
	if got := down.calls.Load(); got != 1 {
		t.Fatalf("the worker whose engine is down was asked %d times by 5 requests; want once, then set aside", got)
	}
	until := r.CooldownUntil("w0")
	if until.IsZero() || time.Until(until) > 10*time.Second {
		t.Fatalf("w0 should be set aside for a moment (one heartbeat interval), until=%v", until)
	}
	if !r.CooldownUntil("w1").IsZero() {
		t.Fatal("the worker that served is not set aside")
	}
}

// An older worker's untyped 502 is the engine's answer, not "nothing": the
// worker is not set aside for it (the configured breaker, when on, still
// counts it as before).
func TestAnOlderWorkersUntyped502IsNotSetAside(t *testing.T) {
	down := newPlainWorker(t, http.StatusBadGateway, "text/plain; charset=utf-8", olderWorkerEngineDown)
	healthy := newSSEWorker(t, delta("hello from w1"), "data: [DONE]\n\n")
	r := workersOf(t, engines.MustNew("vllm", down.srv.URL, ""), healthy)
	_, _ = r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true})
	if !r.CooldownUntil("w0").IsZero() {
		t.Fatal("an engine's own error is not a reason to set the worker aside")
	}
}

// The only worker of a model, set aside, is still asked: the box is a
// preference among workers, never a reason to answer from the leader's own
// engine. While its engine is down the request is unreachable (503 at the
// API); once it answers again it is back in rotation.
func TestTheOnlyWorkerSetAsideIsStillAsked(t *testing.T) {
	r, _ := drainRouter(t, store.Node{ID: "w0", State: store.NodeStateReady})
	down := newPlainWorker(t, http.StatusBadGateway, "application/json", workerEngineDown)
	r.remotes["w0"] = engines.MustNew("vllm", down.srv.URL, "")

	for i := 0; i < 2; i++ {
		_, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m"})
		if !errors.Is(err, engines.ErrWorkerEngineUnreachable) {
			t.Fatalf("request %d: want the worker's engine unreachable, not the local engine, got %v", i, err)
		}
	}
	if down.calls.Load() != 2 {
		t.Fatalf("the only worker must still be asked, got %d calls", down.calls.Load())
	}

	// Its engine is back: the next request is served and the worker leaves the box.
	up := newSSEWorker(t, delta("back"), "data: [DONE]\n\n")
	r.mu.Lock()
	r.remotes["w0"] = engines.MustNew("vllm", up.srv.URL, "")
	r.mu.Unlock()
	stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	if text, _ := drainAnswer(t, stream); text != "back" {
		t.Fatalf("want the worker's answer, got %q", text)
	}
	if !r.CooldownUntil("w0").IsZero() {
		t.Fatal("a worker that answered again must leave the box")
	}
}
