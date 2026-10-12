package router

// A gang's head that answered 200 and then broke its stream before the first
// event has given the client nothing — no token, no byte — so the request is
// served by a sibling gang, the same as a head that could not be dialled.
//
// Measured on the design-partner cell (2026-10-11 18:30:49): a request reached
// gang g1's head while its pods were being replaced; the head answered 200,
// the stream ended `stream: unexpected EOF` before any event, and the client
// got a 502 although gang g0 was serving throughout. A stream that breaks
// AFTER an event went out is never replayed: the caller has part of an answer.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// sseHead is a gang head on the wire. It answers 200 with a chunked SSE body,
// writes the chunks it is given, and then either finishes the stream or drops
// the connection mid-body (no closing chunk: the client reads unexpected EOF).
type sseHead struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func newSSEHead(t *testing.T, chunks []string, finish bool) *sseHead {
	t.Helper()
	h := &sseHead{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
		write := func(s string) { _, _ = fmt.Fprintf(buf, "%x\r\n%s\r\n", len(s), s) }
		for _, c := range chunks {
			write(c)
		}
		if finish {
			write("data: [DONE]\n\n")
			_, _ = buf.WriteString("0\r\n\r\n")
		}
		_ = buf.Flush()
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *sseHead) engine() engines.Engine { return engines.MustNew("llamacpp", h.srv.URL, "") }

func delta(s string) string {
	return `data: {"choices":[{"delta":{"content":"` + s + `"}}]}` + "\n\n"
}

// twoGangs routes model m over g0 and g1, with g0 picked first.
func twoGangs(t *testing.T, g0, g1 *sseHead) *Router {
	t.Helper()
	r, _ := gangRouter(t, "m", map[string][2]string{"g0": {"n1", "n2"}, "g1": {"n3", "n4"}},
		store.Node{ID: "n1"}, store.Node{ID: "n2"}, store.Node{ID: "n3"}, store.Node{ID: "n4"})
	r.remotes["shard:m:g0"], r.remotes["shard:m:g1"] = g0.engine(), g1.engine()
	r.inflight["shard:m:g1"] = 50 // load order alone picks g0 first
	return r
}

func drainAnswer(t *testing.T, stream <-chan engines.StreamEvent) (text string, errEv error) {
	t.Helper()
	var b strings.Builder
	for ev := range stream {
		if ev.Err != nil {
			errEv = ev.Err
		}
		b.WriteString(ev.Delta)
	}
	return b.String(), errEv
}

func TestAStreamBrokenBeforeItsFirstEventIsServedByASiblingGang(t *testing.T) {
	for _, streamed := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", streamed), func(t *testing.T) {
			broken := newSSEHead(t, nil, false)
			healthy := newSSEHead(t, []string{delta("hello from g1")}, true)
			r := twoGangs(t, broken, healthy)

			stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: streamed})
			if err != nil {
				t.Fatalf("the request failed although gang g1 serves the model: %v", err)
			}
			text, evErr := drainAnswer(t, stream)
			if evErr != nil || text != "hello from g1" {
				t.Fatalf("want g1's whole answer, got %q (err %v)", text, evErr)
			}
			if broken.calls.Load() != 1 || healthy.calls.Load() != 1 {
				t.Fatalf("want one attempt on each gang, got g0=%d g1=%d", broken.calls.Load(), healthy.calls.Load())
			}
		})
	}
}

func TestAStreamBrokenAfterItsFirstEventIsNotReplayed(t *testing.T) {
	for _, streamed := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", streamed), func(t *testing.T) {
			broken := newSSEHead(t, []string{delta("half an ans")}, false)
			healthy := newSSEHead(t, []string{delta("hello from g1")}, true)
			r := twoGangs(t, broken, healthy)

			stream, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m", Stream: streamed})
			if err != nil {
				t.Fatalf("the stream had started; its break is reported in the stream: %v", err)
			}
			text, evErr := drainAnswer(t, stream)
			if text != "half an ans" || !errors.Is(evErr, engines.ErrStreamBroken) {
				t.Fatalf("the caller sees what was sent and then the break, as before: %q, %v", text, evErr)
			}
			if healthy.calls.Load() != 0 {
				t.Fatalf("a stream that had sent an event was replayed on g1 (%d calls)", healthy.calls.Load())
			}
		})
	}
}

func TestEveryGangBrokenBeforeItsFirstEventIsAskedOnce(t *testing.T) {
	g0, g1 := newSSEHead(t, nil, false), newSSEHead(t, nil, false)
	r := twoGangs(t, g0, g1)
	_, err := r.Chat(context.Background(), engines.ChatRequest{Model: "m"})
	if !errors.Is(err, engines.ErrBrokeBeforeFirstEvent) {
		t.Fatalf("every gang broke before answering: want ErrBrokeBeforeFirstEvent, got %v", err)
	}
	if g0.calls.Load() != 1 || g1.calls.Load() != 1 {
		t.Fatalf("each gang is asked once, got g0=%d g1=%d", g0.calls.Load(), g1.calls.Load())
	}
}
