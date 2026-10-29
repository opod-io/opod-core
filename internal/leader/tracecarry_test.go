package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/opod-io/opod-sdk/nodeapi"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// T32.42: every HTTP call the leader makes for a traced request is a hop of
// that request's trace — not only the chat it dispatches to a worker (T32.7).

// tracedCtx is a request context under a sampled span, as the leader's
// otelhttp handler leaves it; want is the traceparent a hop must carry.
func tracedCtx(t *testing.T) (ctx context.Context, want string) {
	t.Helper()
	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	sc := trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled})
	return trace.ContextWithSpanContext(context.Background(), sc), "00-" + tid.String() + "-" + sid.String() + "-01"
}

// traceSeen is a fake server that records the traceparent of every request by path.
type traceSeen struct {
	mu  sync.Mutex
	got map[string]string
}

func newTraceSeen(t *testing.T, answer func(w http.ResponseWriter, r *http.Request)) (*traceSeen, *httptest.Server) {
	t.Helper()
	ts := &traceSeen{got: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ts.mu.Lock()
		ts.got[r.URL.Path] = r.Header.Get("traceparent")
		ts.mu.Unlock()
		answer(w, r)
	}))
	t.Cleanup(srv.Close)
	return ts, srv
}

func (ts *traceSeen) at(path string) string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.got[path]
}

// The leader's own calls to a worker — sleep/resume and the pre-route
// tokenize (workerHTTP) — carry the request's trace on.
func TestTheLeadersWorkerCallsCarryTheTraceOn(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	seen, worker := newTraceSeen(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == nodeapi.PathTokenize {
			_, _ = io.WriteString(w, `{"tokens":[1,2,3]}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	n := &store.Node{ID: "w1", Address: worker.URL, WorkerToken: "tok"}
	ctx, want := tracedCtx(t)

	if code, _, err := srv.callWorkerSleep(ctx, n, "resume"); err != nil || code != http.StatusOK {
		t.Fatalf("resume: %d %v", code, err)
	}
	srv.tokenizeOn(ctx, n, engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: "hi"}}})

	for _, path := range []string{"/v1/model/resume", nodeapi.PathTokenize} {
		if got := seen.at(path); got != want {
			t.Errorf("%s reached the worker with traceparent %q, want %q", path, got, want)
		}
	}
}

// The external fallback forward is a hop of the caller's trace.
func TestTheFallbackForwardCarriesTheTraceOn(t *testing.T) {
	srv, _ := newPolicyTestServer(t, &downEngine{})
	seen, fb := newTraceSeen(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"fb-1"}`)
	})
	ctx, want := tracedCtx(t)
	body := []byte(`{"model":"m1","messages":[{"role":"user","content":"hi"}]}`)
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)))
	if !srv.forwardToFallback(httptest.NewRecorder(), r, body, &PolicyRouting{FallbackURL: fb.URL}) {
		t.Fatal("the fallback was not forwarded to")
	}
	if got := seen.at("/v1/chat/completions"); got != want {
		t.Fatalf("the fallback saw traceparent %q, want %q", got, want)
	}
}

// The door starts no span of its own, so the caller's traceparent is the
// trace: it must reach the endpoint exactly as sent, on a name route and on
// an auto route (whose request is a clone).
func TestTheDoorPassesTheCallersTraceOnUntouched(t *testing.T) {
	seen, ep := newTraceSeen(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	})
	_, ts, _ := newTestDoor(t, DoorRoutes{Routes: []DoorRoute{
		{Alias: "a", Upstream: ep.URL},
		{Alias: "auto", Candidates: []DoorCandidate{{Alias: "a", Upstream: ep.URL}}},
	}})
	const sent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	for _, alias := range []string{"a", "auto"} {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(`{"model":"`+alias+`"}`))
		req.Header.Set("traceparent", sent)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", alias, resp.StatusCode)
		}
		if got := seen.at("/v1/chat/completions"); got != sent {
			t.Errorf("%s: the endpoint saw traceparent %q, want the caller's %q", alias, got, sent)
		}
	}
}
