package leader

import (
	"context"
	"log/slog"
	"math/rand"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func remoteParent(sampled bool) context.Context {
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	cfg := trace.SpanContextConfig{TraceID: tid, SpanID: sid, Remote: true}
	if sampled {
		cfg.TraceFlags = trace.FlagsSampled
	}
	return trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(cfg))
}

// T32.8: ratio 0 records no trace the leader starts, and still records every
// request that arrives with a sampled parent (LiteLLM's decision wins); an
// unsampled parent is followed too, whatever the ratio.
func TestTraceSampleRatioZeroKeepsASampledParent(t *testing.T) {
	sampler, _, err := traceSampler("0")
	if err != nil {
		t.Fatal(err)
	}
	exp := tracetest.NewInMemoryExporter()
	tr := tracesdk.NewTracerProvider(tracesdk.WithSyncer(exp), tracesdk.WithSampler(sampler)).Tracer("t")

	_, root := tr.Start(context.Background(), "root")
	root.End()
	_, child := tr.Start(remoteParent(true), "sampled-parent")
	child.End()
	_, dropped := tr.Start(remoteParent(false), "unsampled-parent")
	dropped.End()

	got := map[string]bool{}
	for _, s := range exp.GetSpans().Snapshots() {
		got[s.Name()] = true
	}
	if got["root"] {
		t.Error("ratio 0 recorded a root span")
	}
	if !got["sampled-parent"] {
		t.Error("ratio 0 dropped a request whose caller sampled it; the parent's decision must win")
	}
	if got["unsampled-parent"] {
		t.Error("a request whose caller did not sample it was recorded")
	}
}

// 0.1 keeps about a tenth of the traces the leader starts, decided by trace id.
func TestTraceSampleRatioKeepsItsShareOfRootTraces(t *testing.T) {
	sampler, ratio, err := traceSampler("0.1")
	if err != nil || ratio != 0.1 {
		t.Fatalf("traceSampler(0.1) = %v, %v", ratio, err)
	}
	rng := rand.New(rand.NewSource(32))
	kept := 0
	const n = 10000
	for i := 0; i < n; i++ {
		var tid trace.TraceID
		rng.Read(tid[:])
		res := sampler.ShouldSample(tracesdk.SamplingParameters{ParentContext: context.Background(), TraceID: tid, Name: "root"})
		if res.Decision == tracesdk.RecordAndSample {
			kept++
		}
	}
	if kept < 900 || kept > 1100 {
		t.Fatalf("ratio 0.1 kept %d of %d root traces, want about 1000", kept, n)
	}
}

// Unset is every request, as before the variable existed.
func TestTraceSampleRatioUnsetRecordsEverything(t *testing.T) {
	sampler, ratio, err := traceSampler("")
	if err != nil || ratio != 1 {
		t.Fatalf("traceSampler(\"\") = %v, %v; want 1", ratio, err)
	}
	var tid trace.TraceID
	for i := 0; i < 100; i++ {
		tid[0], tid[15] = byte(i), byte(255-i)
		if sampler.ShouldSample(tracesdk.SamplingParameters{ParentContext: context.Background(), TraceID: tid}).Decision != tracesdk.RecordAndSample {
			t.Fatal("unset dropped a root trace")
		}
	}
}

// A value that is not a ratio fails the start and names the variable — also
// when no collector is configured yet.
func TestTraceSampleRatioRefusesWhatIsNotARatio(t *testing.T) {
	for _, v := range []string{"x", "-0.1", "1.5", "NaN", "10%"} {
		if _, _, err := traceSampler(v); err == nil || !strings.Contains(err.Error(), "OPOD_TRACE_SAMPLE_RATIO") {
			t.Errorf("traceSampler(%q) = %v, want an error naming OPOD_TRACE_SAMPLE_RATIO", v, err)
		}
	}
	if _, err := initTracing(context.Background(), "", "2", "dev", slog.Default()); err == nil {
		t.Error("initTracing took an invalid ratio with tracing off; a typo must fail where it was made")
	}
}

// The ratio is the one the leader's provider runs with: with ratio 0 a root
// span the leader starts does not record, and a sampled caller's does.
func TestInitTracingRunsWithTheRatio(t *testing.T) {
	shutdown, err := initTracing(context.Background(), "127.0.0.1:1", "0", "dev", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) }) // a shut-down provider records nothing for later tests
	tr := otel.GetTracerProvider().Tracer("t")
	_, root := tr.Start(context.Background(), "root")
	defer root.End()
	if root.IsRecording() {
		t.Error("ratio 0: the leader's provider recorded a root span")
	}
	_, child := tr.Start(remoteParent(true), "child")
	defer child.End()
	if !child.IsRecording() {
		t.Error("ratio 0: the leader's provider dropped a request its caller sampled")
	}
}
