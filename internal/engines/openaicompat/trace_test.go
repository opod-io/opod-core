package openaicompat

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The engines package's tracer delegates to the FIRST global provider set in
// a process, so this test binary sets one, once, and reads it back through
// the exporter.
var (
	traceOnce sync.Once
	traceExp  *tracetest.InMemoryExporter
	traceTP   *sdktrace.TracerProvider
)

func recordedSpans(t *testing.T) (*sdktrace.TracerProvider, *tracetest.InMemoryExporter) {
	t.Helper()
	traceOnce.Do(func() {
		traceExp = tracetest.NewInMemoryExporter()
		traceTP = sdktrace.NewTracerProvider(sdktrace.WithSyncer(traceExp))
		otel.SetTracerProvider(traceTP)
	})
	traceExp.Reset()
	return traceTP, traceExp
}

// T32.7 + T32.6 on the driver span: the engine receives the trace of the
// leader's request, parented on the driver's own span, with no event per
// chunk; and the span says how many prompt tokens the engine served from
// its prefix cache when the engine reports it.
func TestTheEngineCallContinuesTheLeadersTrace(t *testing.T) {
	tp, exp := recordedSpans(t)
	var traceparent string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		traceparent = r.Header.Get("traceparent")
		w.Header().Set("Content-Type", "text/event-stream")
		for _, d := range []string{"a", "b", "c"} {
			fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", d)
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15,\"prompt_tokens_details\":{\"cached_tokens\":8}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	ctx, root := tp.Tracer("test").Start(context.Background(), "leader request")
	c := NewClient("vllm", srv.URL, nil)
	ch, err := c.Chat(ctx, engines.ChatRequest{Model: "m", Stream: true, Messages: []engines.Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	var usage *engines.Usage
	for ev := range ch {
		if ev.Done {
			usage = ev.Usage
		}
	}
	root.End()

	// The driver span ends just after the stream's channel closes (its End is
	// deferred behind the close), so it may reach the exporter a moment later.
	var driver sdktrace.ReadOnlySpan
	for deadline := time.Now().Add(2 * time.Second); driver == nil; {
		for _, s := range exp.GetSpans().Snapshots() {
			if s.Name() == "vllm.Chat" {
				driver = s
			}
		}
		if driver == nil && time.Now().After(deadline) {
			t.Fatal("no vllm.Chat span was recorded")
		}
		time.Sleep(time.Millisecond)
	}
	want := "00-" + root.SpanContext().TraceID().String() + "-" + driver.SpanContext().SpanID().String() + "-01"
	if traceparent != want {
		t.Fatalf("the engine saw traceparent %q, want %q (the leader's trace, parented on the driver span)", traceparent, want)
	}
	if n := len(driver.Events()); n != 0 {
		t.Fatalf("the driver span carries %d events; the decision is header only, nothing per chunk", n)
	}
	if usage == nil || usage.CachedTokens != 8 {
		t.Fatalf("usage %+v, want the engine's 8 cached tokens", usage)
	}
	if v, ok := attr(driver.Attributes(), "opod.tokens.cached"); !ok || v.AsInt64() != 8 {
		t.Fatalf("opod.tokens.cached = %v (set %v), want 8", v, ok)
	}
}

func attr(kvs []attribute.KeyValue, key string) (attribute.Value, bool) {
	for _, kv := range kvs {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}
