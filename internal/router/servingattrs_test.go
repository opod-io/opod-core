package router

import (
	"context"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// recordRouterSpans swaps the router's tracer for one that records into an
// in-memory exporter, for the length of the test.
func recordRouterSpans(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp))
	prev := tracer
	tracer = tp.Tracer("test")
	t.Cleanup(func() { tracer = prev })
	return exp
}

func spanAttrs(exp *tracetest.InMemoryExporter, name string) map[string]attribute.Value {
	for _, s := range exp.GetSpans().Snapshots() {
		if s.Name() == name {
			m := map[string]attribute.Value{}
			for _, kv := range s.Attributes() {
				m[string(kv.Key)] = kv.Value
			}
			return m
		}
	}
	return nil
}

// T32.6: a traced request's attempt span says what served it — the worker
// (opod.node_id; for a gang, its head worker), the gang when the target is a gang's head, and the
// admission class that ranked the request. The model id carries a ':' on
// purpose: the gang is read by matching the model whole, never by a split.
func TestTheAttemptSpanSaysWhichGangAndClassServed(t *testing.T) {
	exp := recordRouterSpans(t)
	const model = "llama3.2:1b"
	r, _ := gangRouter(t, model, map[string][2]string{"g7": {"n1", "n2"}},
		store.Node{ID: "n1"}, store.Node{ID: "n2"})
	r.remotes[gangKey(model, "g7")] = &stubEngine{name: "g7"}

	ctx := WithTrace(context.Background())
	NoteClass(ctx, "sheddable")
	stream, err := r.Chat(ctx, engines.ChatRequest{Model: model})
	if err != nil {
		t.Fatal(err)
	}
	for range stream {
	}

	a := spanAttrs(exp, "router.Chat.attempt")
	if a == nil {
		t.Fatal("no router.Chat.attempt span was recorded")
	}
	if got := a["opod.gang.id"].AsString(); got != "g7" {
		t.Errorf("opod.gang.id = %q, want g7", got)
	}
	if got := a["opod.class"].AsString(); got != "sheddable" {
		t.Errorf("opod.class = %q, want sheddable", got)
	}
	// T32.42: the span names the worker that heads the gang, not the gang's
	// pseudo node; the usage row still counts the request under the key.
	if got := a["opod.node_id"].AsString(); got != "n1" {
		t.Errorf("opod.node_id = %q, want the gang's head worker n1", got)
	}
	if got := NodeFrom(ctx); got != gangKey(model, "g7") {
		t.Errorf("usage node = %q, want the gang's key", got)
	}
}

// A worker that is not a gang's head gets no gang attribute.
func TestAWorkerAttemptCarriesNoGang(t *testing.T) {
	exp := tracetest.NewInMemoryExporter()
	_, sp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)).Tracer("t").Start(context.Background(), "attempt")
	servingAttrs(context.Background(), sp, "m", "n1")
	sp.End()
	if _, ok := spanAttrs(exp, "attempt")["opod.gang.id"]; ok {
		t.Fatal("a worker's attempt was given a gang")
	}
}

// countingSpan is a span that does not record and counts what it is asked to
// set — what the attempt span is when tracing is off or the sampler dropped
// the request.
type countingSpan struct {
	noop.Span
	set int
}

func (s *countingSpan) IsRecording() bool                   { return false }
func (s *countingSpan) SetAttributes(...attribute.KeyValue) { s.set++ }

// With tracing off nothing is computed: no class read, no gang matched, no
// attribute built.
func TestServingAttrsComputeNothingWhenTheSpanDoesNotRecord(t *testing.T) {
	ctx := WithTrace(context.Background())
	NoteClass(ctx, "critical")
	sp := &countingSpan{}
	servingAttrs(ctx, trace.Span(sp), "m", gangKey("m", "g0"))
	if sp.set != 0 {
		t.Fatalf("SetAttributes was called %d times on a span that does not record", sp.set)
	}
}
