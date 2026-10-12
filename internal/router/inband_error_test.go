package router

// A worker relays its own engine's failure as an in-band error chunk after its
// 200 (agent.writeSSE). Before the first event that is still a request that
// reached the caller as nothing: when the worker marks it a transport break of
// its engine (engines.StreamBrokenType), a sibling worker serves it, as one
// whose connection broke would be (T8.17). Any other in-band error is the
// engine's own answer and is never replayed (PLAN T8.18).

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// sseWorker answers 200, writes a worker's role chunk and then the chunks it
// is given, and ends the body cleanly — what agent.writeSSE does after an
// engine error.
type sseWorker struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newSSEWorker(t *testing.T, chunks ...string) *sseWorker {
	t.Helper()
	w := &sseWorker{}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.calls.Add(1)
		rw.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(rw, `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`+"\n\n")
		for _, c := range chunks {
			_, _ = io.WriteString(rw, c)
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func twoWorkers(t *testing.T, w0, w1 *sseWorker) *Router {
	t.Helper()
	r, _ := drainRouter(t,
		store.Node{ID: "w0", State: store.NodeStateReady},
		store.Node{ID: "w1", State: store.NodeStateReady})
	r.remotes["w0"] = engines.MustNew("llamacpp", w0.srv.URL, "")
	r.remotes["w1"] = engines.MustNew("llamacpp", w1.srv.URL, "")
	r.inflight["w1"] = 50 // load order alone picks w0 first
	return r
}

const (
	workerEngineBroke = `data: {"error":{"message":"stream: unexpected EOF","type":"stream_broken"}}` + "\n\n"
	workerEngineSaid  = `data: {"error":{"message":"prompt is too long","type":"BadRequestError"}}` + "\n\n"
)

func TestAWorkerWhoseEngineBrokeBeforeTheFirstEventIsServedByASibling(t *testing.T) {
	broken := newSSEWorker(t, workerEngineBroke)
	healthy := newSSEWorker(t, delta("hello from w1"), "data: [DONE]\n\n")
	r := twoWorkers(t, broken, healthy)

	stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("the request failed although worker w1 serves the model: %v", err)
	}
	text, evErr := drainAnswer(t, stream)
	if evErr != nil || text != "hello from w1" {
		t.Fatalf("want w1's whole answer, got %q (err %v)", text, evErr)
	}
	if broken.calls.Load() != 1 || healthy.calls.Load() != 1 {
		t.Fatalf("want one attempt on each worker, got w0=%d w1=%d", broken.calls.Load(), healthy.calls.Load())
	}

	// Every worker's engine broke: no capacity right now, each asked once.
	other := newSSEWorker(t, workerEngineBroke)
	r = twoWorkers(t, newSSEWorker(t, workerEngineBroke), other)
	if _, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m"}); !errors.Is(err, engines.ErrBrokeBeforeFirstEvent) {
		t.Fatalf("every worker's engine broke before answering: want ErrBrokeBeforeFirstEvent, got %v", err)
	}
	if other.calls.Load() != 1 {
		t.Fatalf("the second worker is asked once, got %d", other.calls.Load())
	}
}

func TestAWorkerEnginesOwnInBandErrorIsNotReplayed(t *testing.T) {
	for name, chunk := range map[string]string{
		"typed by the engine":  workerEngineSaid,
		"from an older worker": `data: {"error":{"message":"stream: unexpected EOF"}}` + "\n\n",
	} {
		t.Run(name, func(t *testing.T) {
			said := newSSEWorker(t, chunk)
			healthy := newSSEWorker(t, delta("hello from w1"), "data: [DONE]\n\n")
			r := twoWorkers(t, said, healthy)

			stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true})
			if err != nil {
				t.Fatalf("the engine's answer reaches the caller in the stream: %v", err)
			}
			text, evErr := drainAnswer(t, stream)
			if text != "" || !errors.Is(evErr, engines.ErrStreamErrored) {
				t.Fatalf("want the engine's error and no text, got %q, %v", text, evErr)
			}
			if healthy.calls.Load() != 0 {
				t.Fatalf("an engine's own error was replayed on w1 (%d calls)", healthy.calls.Load())
			}
		})
	}
}

func TestAWorkerWhoseEngineBrokeAfterTheFirstEventIsNotReplayed(t *testing.T) {
	broken := newSSEWorker(t, delta("half an ans"), workerEngineBroke)
	healthy := newSSEWorker(t, delta("hello from w1"), "data: [DONE]\n\n")
	r := twoWorkers(t, broken, healthy)

	stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: true})
	if err != nil {
		t.Fatalf("the stream had started; its break is reported in the stream: %v", err)
	}
	text, evErr := drainAnswer(t, stream)
	if text != "half an ans" || !errors.Is(evErr, engines.ErrStreamBroken) {
		t.Fatalf("the caller sees what was sent and then the break, never a clean end: %q, %v", text, evErr)
	}
	if healthy.calls.Load() != 0 {
		t.Fatalf("a stream that had sent an event was replayed on w1 (%d calls)", healthy.calls.Load())
	}
}
