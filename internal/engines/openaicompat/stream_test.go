package openaicompat

import (
	"context"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

func collect(ctx context.Context, sse string, span bool) []engines.StreamEvent {
	out := make(chan engines.StreamEvent, 64)
	body := io.NopCloser(strings.NewReader(sse))
	if span {
		_, sp := engines.StartChatSpan(ctx, "vllm", "m", "http://x", 1)
		ConsumeStreamWithSpan(ctx, body, out, sp)
	} else {
		ConsumeStream(ctx, body, out)
	}
	var evs []engines.StreamEvent
	for ev := range out {
		evs = append(evs, ev)
	}
	return evs
}

// The two entry points are one loop (PLAN T15.9): every shape a server sends
// yields the same events with or without a span — the usage-only chunk after
// finish_reason (vLLM, MLX), a bare [DONE], and a broken chunk mid-stream.
func TestConsumeStreamShapesAreTheSameWithAndWithoutASpan(t *testing.T) {
	cases := map[string]struct {
		sse           string
		deltas        int
		reason, final string
		usage         int
		err           bool
	}{
		"usage after finish_reason": {
			sse: "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"b\"},\"finish_reason\":\"stop\"}]}\n" +
				"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n" +
				"data: [DONE]\n",
			deltas: 2, reason: "stop", usage: 5,
		},
		"bare [DONE]": {
			sse:    "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\ndata: [DONE]\n",
			deltas: 1, reason: "",
		},
		"stream ends without [DONE]": {
			sse:    "data: {\"choices\":[{\"delta\":{\"content\":\"a\"},\"finish_reason\":\"length\"}]}\n",
			deltas: 1, reason: "length",
		},
		"broken chunk mid-stream": {
			sse:    "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\ndata: {not json\n",
			deltas: 1, err: true,
		},
	}
	for name, c := range cases {
		for _, span := range []bool{false, true} {
			evs := collect(context.Background(), c.sse, span)
			deltas, done, errs := 0, 0, 0
			var reason string
			usage := 0
			for _, ev := range evs {
				switch {
				case ev.Err != nil:
					errs++
				case ev.Done:
					done++
					reason = ev.Reason
					if ev.Usage != nil {
						usage = ev.Usage.TotalTokens
					}
				default:
					deltas++
				}
			}
			if deltas != c.deltas || (errs > 0) != c.err || reason != c.reason || usage != c.usage {
				t.Errorf("%s (span=%v): deltas=%d errs=%d reason=%q usage=%d: %+v", name, span, deltas, errs, reason, usage, evs)
			}
			if !c.err && done != 1 {
				t.Errorf("%s (span=%v): exactly one Done, got %d", name, span, done)
			}
		}
	}
}

// A cancelled stream leaves no goroutine behind and still closes out, on both
// entry points. The span variant used to start a relay goroutine and, on
// cancel, a second one to drain it.
func TestACancelledStreamLeavesNoGoroutine(t *testing.T) {
	for _, span := range []bool{false, true} {
		pr, pw := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		out := make(chan engines.StreamEvent)
		before := runtime.NumGoroutine()
		done := make(chan struct{})
		go func() {
			defer close(done)
			if span {
				_, sp := engines.StartChatSpan(ctx, "vllm", "m", "http://x", 1)
				ConsumeStreamWithSpan(ctx, pr, out, sp)
			} else {
				ConsumeStream(ctx, pr, out)
			}
		}()
		_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n"))
		<-out // the consumer took one event, then leaves
		cancel()
		_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n")) // producer keeps going
		_ = pw.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("span=%v: the consumer did not return after cancel", span)
		}
		if _, open := <-out; open {
			t.Fatalf("span=%v: out was not closed", span)
		}
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if n := runtime.NumGoroutine(); n > before {
			t.Fatalf("span=%v: %d goroutines before, %d after a cancelled stream", span, before, n)
		}
	}
}
