package scheduler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// T32.42: a model load or unload the leader sends for a traced request (an
// admin call under the leader's otelhttp span) carries that trace on, on both
// of the orchestrator's clients.
func TestALoadAndAnUnloadCarryTheTraceOn(t *testing.T) {
	var mu sync.Mutex
	got := map[string]string{}
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path] = r.Header.Get("traceparent")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/model/unload" {
			_, _ = io.WriteString(w, `{"status":"unloaded","model":"m"}`)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	}))
	defer worker.Close()

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Nodes().Upsert(context.Background(), store.Node{ID: "w1", Address: strings.TrimPrefix(worker.URL, "http://"),
		WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())

	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))
	want := "00-" + tid.String() + "-" + sid.String() + "-01"

	entry := models.Entry{ID: "m", Source: models.SourceSpec{Type: "huggingface", Repo: "example/M"}}
	if err := o.PlaceOnNodes(ctx, entry, []string{"w1"}, false, false); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, err := o.UnloadFromNode(ctx, entry, "w1"); err != nil {
		t.Fatalf("unload: %v", err)
	}
	for _, path := range []string{"/v1/model/load", "/v1/model/unload"} {
		if got[path] != want {
			t.Errorf("%s reached the worker with traceparent %q, want %q", path, got[path], want)
		}
	}
}
