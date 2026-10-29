package agent

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"go.opentelemetry.io/otel/trace"
)

// T32.7 / T32.6 on the hop that matters in a cluster: leader → worker →
// engine. The worker exports no spans, so the trace survives only if it reads
// the leader's traceparent on the way in and its engine call carries it on;
// and the engine's cached-token count reaches the leader only if the worker
// passes it on in its own usage chunk.
func TestTheLeadersTraceReachesTheEngineThroughTheWorker(t *testing.T) {
	var traceparent string
	eng := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			return
		}
		traceparent = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":1,\"total_tokens\":21,\"prompt_tokens_details\":{\"cached_tokens\":16}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(eng.Close)

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	worker := &Server{Engine: engines.MustNew("vllm", eng.URL, ""), Token: "tok"}
	go func() { _ = worker.Start(ctx, addr) }()
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			_ = c.Close()
			break
		}
		if i == 200 {
			t.Fatal("the worker never listened")
		}
		time.Sleep(10 * time.Millisecond)
	}

	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	leaderCtx := trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	// The leader's side: the same client the router dials a worker with.
	leaderSide := engines.MustNew("vllm", "http://"+addr, "tok")
	ch, err := leaderSide.Chat(leaderCtx, engines.ChatRequest{Model: "m", Stream: true, Messages: []engines.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var usage *engines.Usage
	for ev := range ch {
		if ev.Done {
			usage = ev.Usage
		}
	}

	if want := "00-" + tid.String() + "-" + sid.String() + "-01"; traceparent != want {
		t.Fatalf("the engine behind the worker saw traceparent %q, want the leader's %q", traceparent, want)
	}
	if usage == nil || usage.CachedTokens != 16 {
		t.Fatalf("the leader read usage %+v, want the engine's 16 cached tokens passed on by the worker", usage)
	}
}
