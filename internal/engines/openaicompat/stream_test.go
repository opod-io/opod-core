package openaicompat

import (
	"context"
	"errors"
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

// A tool call arrives as `delta.tool_calls` fragments. The reader forwards
// each one whole and invents nothing: merging is the aggregating caller's
// job, and a streaming caller is owed the fragments as the engine sent them
// (T10.8).
func TestConsumeStreamForwardsToolCallFragments(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"role":"assistant"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":""}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}}]}

data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`
	evs := collect(context.Background(), sse, false)

	var frags []string
	var done *engines.StreamEvent
	for i, ev := range evs {
		if len(ev.ToolCalls) > 0 {
			frags = append(frags, string(ev.ToolCalls))
		}
		if ev.Done {
			done = &evs[i]
		}
		if ev.Delta != "" {
			t.Fatalf("a tool-call chunk produced text: %q", ev.Delta)
		}
	}
	if len(frags) != 2 {
		t.Fatalf("got %d tool-call events, want 2: %+v", len(frags), evs)
	}
	if !strings.Contains(frags[0], `"name":"f"`) || !strings.Contains(frags[1], `"arguments":"{}"`) {
		t.Fatalf("fragments were altered: %v", frags)
	}
	if done == nil || done.Reason != "tool_calls" {
		t.Fatalf("the finish reason the caller branches on was lost: %+v", done)
	}
}

// A body that ends in a transport failure is classified, so the router can
// tell "the engine's stream broke" from an engine's own error.
func TestAStreamThatBreaksIsClassifiedAsBroken(t *testing.T) {
	out := make(chan engines.StreamEvent, 4)
	ConsumeStream(context.Background(), io.NopCloser(io.MultiReader(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n"), errReader{io.ErrUnexpectedEOF})), out)
	var last engines.StreamEvent
	for ev := range out {
		last = ev
	}
	if !errors.Is(last.Err, engines.ErrStreamBroken) || !strings.HasPrefix(last.Err.Error(), "stream: ") {
		t.Fatalf("want a broken-stream error that still reads \"stream: …\", got %v", last.Err)
	}
	// A chunk that does not decode is the engine's own fault, not a break.
	out = make(chan engines.StreamEvent, 4)
	ConsumeStream(context.Background(), io.NopCloser(strings.NewReader("data: {not json\n")), out)
	for ev := range out {
		last = ev
	}
	if last.Err == nil || errors.Is(last.Err, engines.ErrStreamBroken) {
		t.Fatalf("a decode error is not a broken stream: %v", last.Err)
	}
}

type errReader struct{ err error }

func (e errReader) Read([]byte) (int, error) { return 0, e.err }

// A server's own error event in the stream ends it as an error, never as the
// clean end the close that follows would otherwise synthesize (PLAN T8.18).
func TestAnInBandErrorEventEndsTheStreamAsAnError(t *testing.T) {
	const role = `data: {"choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}` + "\n\n"
	for name, tc := range map[string]struct {
		chunk  string
		broken bool // read back as a transport break (engines.ErrStreamBroken)
		says   string
	}{
		"a worker's relayed engine break": {`data: {"error":{"message":"stream: unexpected EOF","type":"stream_broken"}}`, true, "unexpected EOF"},
		"a worker older than the type":    {`data: {"error":{"message":"stream: unexpected EOF"}}`, false, "unexpected EOF"},
		"llama-server's shape, int code":  {`data: {"error":{"code":500,"message":"slot failed","type":"server_error"}}`, false, "slot failed"},
		"vLLM's shape, string code":       {`data: {"error":{"object":"error","message":"boom","type":"InternalServerError","code":"500"}}`, false, "boom"},
		"a bare string":                   {`data: {"error":"out of memory"}`, false, "out of memory"},
	} {
		t.Run(name, func(t *testing.T) {
			out := make(chan engines.StreamEvent, 4)
			ConsumeStream(context.Background(), io.NopCloser(strings.NewReader(role+tc.chunk+"\n\n")), out)
			var evs []engines.StreamEvent
			for ev := range out {
				evs = append(evs, ev)
			}
			if len(evs) != 1 || evs[0].Done || evs[0].Err == nil {
				t.Fatalf("want exactly one error event and no clean end, got %+v", evs)
			}
			err := evs[0].Err
			if errors.Is(err, engines.ErrStreamBroken) != tc.broken || errors.Is(err, engines.ErrStreamErrored) == tc.broken {
				t.Fatalf("broken=%v expected, got %v", tc.broken, err)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the server's words must survive: %v", err)
			}
		})
	}
	// "error": null is no error.
	out := make(chan engines.StreamEvent, 4)
	ConsumeStream(context.Background(), io.NopCloser(strings.NewReader(`data: {"error":null,"choices":[{"delta":{"content":"x"}}]}`+"\n\ndata: [DONE]\n\n")), out)
	var text string
	for ev := range out {
		if ev.Err != nil {
			t.Fatalf("a null error is no error: %v", ev.Err)
		}
		text += ev.Delta
	}
	if text != "x" {
		t.Fatalf("the content beside a null error is kept: %q", text)
	}
}
