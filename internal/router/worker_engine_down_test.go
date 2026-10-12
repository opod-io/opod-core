package router

// A worker whose own engine is down (killed, restarting) answers 502 with a
// typed body (engines.EngineUnreachableType): it was reached, its engine was
// not, and nothing of an answer exists. The leader used to read any non-2xx
// from a worker as the engine's error, so a sibling serving the same model was
// never asked (design-partner cell, PLAN T8.18: every queued request after an
// engine kill got a 502).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// plainWorker answers every request with status and body, as a worker does
// when its engine call fails before any answer.
type plainWorker struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newPlainWorker(t *testing.T, status int, contentType, body string) *plainWorker {
	t.Helper()
	w := &plainWorker{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.calls.Add(1)
		rw.Header().Set("Content-Type", contentType)
		rw.WriteHeader(status)
		_, _ = rw.Write([]byte(body))
	}))
	t.Cleanup(w.srv.Close)
	return w
}

const (
	// What a worker of this version answers when its engine refuses the dial.
	workerEngineDown = `{"error":{"type":"engine_unreachable_on_worker","message":"engine: llamacpp unreachable at http://127.0.0.1:8089: dial tcp 127.0.0.1:8089: connect: connection refused"}}`
	// What an older worker answered, and still does: plain text.
	olderWorkerEngineDown = "engine: llamacpp unreachable at http://127.0.0.1:8089: dial tcp 127.0.0.1:8089: connect: connection refused\n"
)

func workersOf(t *testing.T, first engines.Engine, healthy *sseWorker) *Router {
	t.Helper()
	r, _ := drainRouter(t,
		store.Node{ID: "w0", State: store.NodeStateReady},
		store.Node{ID: "w1", State: store.NodeStateReady})
	r.remotes["w0"] = first
	r.remotes["w1"] = engines.MustNew("vllm", healthy.srv.URL, "")
	r.inflight["w1"] = 50 // load order alone picks w0 first
	return r
}

func TestAWorkerWhoseEngineIsDownIsServedByASibling(t *testing.T) {
	for _, streamed := range []bool{true, false} {
		down := newPlainWorker(t, http.StatusBadGateway, "application/json", workerEngineDown)
		healthy := newSSEWorker(t, delta("hello from w1"), "data: [DONE]\n\n")
		r := workersOf(t, engines.MustNew("vllm", down.srv.URL, ""), healthy)

		stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: streamed})
		if err != nil {
			t.Fatalf("stream=%v: the request failed although worker w1 serves the model: %v", streamed, err)
		}
		if text, evErr := drainAnswer(t, stream); evErr != nil || text != "hello from w1" {
			t.Fatalf("stream=%v: want w1's whole answer, got %q (err %v)", streamed, text, evErr)
		}
		if down.calls.Load() != 1 || healthy.calls.Load() != 1 {
			t.Fatalf("stream=%v: want one attempt on each worker, got w0=%d w1=%d", streamed, down.calls.Load(), healthy.calls.Load())
		}
	}
}

// Every worker's engine down: each is asked once and the answer is
// "unreachable", which the API layer turns into 503 + Retry-After.
func TestEveryWorkersEngineDownIsUnreachable(t *testing.T) {
	a := newPlainWorker(t, http.StatusBadGateway, "application/json", workerEngineDown)
	b := newPlainWorker(t, http.StatusBadGateway, "application/json", workerEngineDown)
	r, _ := drainRouter(t,
		store.Node{ID: "w0", State: store.NodeStateReady},
		store.Node{ID: "w1", State: store.NodeStateReady})
	r.remotes["w0"] = engines.MustNew("vllm", a.srv.URL, "")
	r.remotes["w1"] = engines.MustNew("vllm", b.srv.URL, "")
	_, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m"})
	if !errors.Is(err, engines.ErrUnreachable) || !errors.Is(err, engines.ErrWorkerEngineUnreachable) {
		t.Fatalf("want the workers' engines unreachable, got %v", err)
	}
	if a.calls.Load() != 1 || b.calls.Load() != 1 {
		t.Fatalf("each worker is asked once, got w0=%d w1=%d", a.calls.Load(), b.calls.Load())
	}
}

// An older worker's plain-text 502 is read as before: the engine's error,
// shown, never replayed.
func TestAnOlderWorkersEngineDownIsReadAsBefore(t *testing.T) {
	down := newPlainWorker(t, http.StatusBadGateway, "text/plain; charset=utf-8", olderWorkerEngineDown)
	healthy := newSSEWorker(t, delta("hello from w1"), "data: [DONE]\n\n")
	r := workersOf(t, engines.MustNew("vllm", down.srv.URL, ""), healthy)

	_, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true})
	if !errors.Is(err, engines.ErrUpstream) || errors.Is(err, engines.ErrUnreachable) {
		t.Fatalf("an untyped 502 is an engine error, as it always was: %v", err)
	}
	if healthy.calls.Load() != 0 {
		t.Fatalf("an untyped 502 was replayed on w1 (%d calls)", healthy.calls.Load())
	}
}
