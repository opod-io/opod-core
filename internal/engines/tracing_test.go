package engines

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

// sampledParent is a span context as a traced leader request carries it.
func sampledParent(t *testing.T) (context.Context, trace.SpanContext) {
	t.Helper()
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), sc), sc
}

// T32.7: a request made under a span leaves with that span's traceparent; one
// made under none leaves untouched, and the caller's request is never edited.
func TestTraceTransportCarriesTheTraceOn(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("traceparent"))
	}))
	t.Cleanup(srv.Close)
	c := &http.Client{Transport: TraceTransport(nil)}

	ctx, sc := sampledParent(t)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if req.Header.Get("traceparent") != "" {
		t.Fatal("the transport wrote into the caller's request; a RoundTripper must not modify it")
	}

	plain, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if resp, err = c.Do(plain); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	want := "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-01"
	if len(got) != 2 || got[0] != want {
		t.Fatalf("the engine saw traceparent %q, want %q", got, want)
	}
	if got[1] != "" {
		t.Fatalf("a request under no span must carry no traceparent, got %q", got[1])
	}
}

type closeCounter struct {
	http.RoundTripper
	closed int
}

func (c *closeCounter) CloseIdleConnections() { c.closed++ }

// The wrapper must not swallow CloseIdleConnections: the router releases an
// evicted engine's pooled sockets through it (T15.16).
func TestTraceTransportPassesCloseIdleConnectionsOn(t *testing.T) {
	base := &closeCounter{RoundTripper: http.DefaultTransport}
	(&http.Client{Transport: TraceTransport(base)}).CloseIdleConnections()
	if base.closed != 1 {
		t.Fatalf("CloseIdleConnections reached the base transport %d times, want 1", base.closed)
	}
}

// The worker's side of the hop: the leader's traceparent becomes the parent
// span context of the request the worker serves.
func TestContinueTraceMakesTheCallersTraceTheParent(t *testing.T) {
	_, sc := sampledParent(t)
	var seen trace.SpanContext
	h := ContinueTrace(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = trace.SpanContextFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("traceparent", "00-"+sc.TraceID().String()+"-"+sc.SpanID().String()+"-01")
	h.ServeHTTP(httptest.NewRecorder(), req)
	if seen.TraceID() != sc.TraceID() || seen.SpanID() != sc.SpanID() || !seen.IsSampled() || !seen.IsRemote() {
		t.Fatalf("the handler's span context is %+v, want the caller's %+v (remote)", seen, sc)
	}
}
