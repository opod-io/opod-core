package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// A request for a model whose only holder sleeps resumes that worker, is held
// through the wake and finds the model servable — one resume however many
// callers arrive together, and the placement routable on the worker's own 200
// rather than on its next heartbeat. A worker with no sleep mode (501) and a
// resume that outlasts the budget both leave the caller with the 503.
func TestSleepingWorkerIsResumedByTheRequest(t *testing.T) {
	ctx := context.Background()
	newLeader := func(t *testing.T, resume http.HandlerFunc) (*Server, store.Store) {
		t.Helper()
		cfg := config.Default()
		cfg.Listen = ":0"
		cfg.Router.HeartbeatMaxAgeSeconds = 30
		st, err := store.OpenSQLite(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/model/resume" {
				resume(w, r)
				return
			}
			http.NotFound(w, r)
		}))
		t.Cleanup(worker.Close)
		if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", State: "ready", Address: strings.TrimPrefix(worker.URL, "http://"), WorkerToken: "wtok", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
		rec := httptest.NewRecorder()
		srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat", strings.NewReader(`{"id":"w1","loaded_models":["m"],"sleeping":true}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
		}
		if reason := srv.unavailable(ctx, "m"); reason != wakingMessage {
			t.Fatalf("a sleeping holder must read as waking, got %q", reason)
		}
		return srv, st
	}

	t.Run("resumed, held and served; one resume for a burst", func(t *testing.T) {
		var calls atomic.Int32
		srv, st := newLeader(t, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			time.Sleep(150 * time.Millisecond) // a wake takes a moment
			_, _ = w.Write([]byte(`{"status":"awake"}`))
		})
		var wg sync.WaitGroup
		got := make([]bool, 6)
		for i := range got {
			wg.Add(1)
			go func(i int) { defer wg.Done(); got[i] = srv.wakeForRequest(ctx, "m") }(i)
		}
		wg.Wait()
		for i, ok := range got {
			if !ok {
				t.Fatalf("caller %d was not served after the wake", i)
			}
		}
		if n := calls.Load(); n != 1 {
			t.Fatalf("six callers sent %d resumes, want one shared", n)
		}
		ps, _ := st.Placements().GetByNode(ctx, "w1")
		if len(ps) != 1 || ps[0].Status != "ready" {
			t.Fatalf("the placement must be routable on the worker's 200, not on its next heartbeat: %+v", ps)
		}
		if reason := srv.unavailable(ctx, "m"); reason != "" {
			t.Fatalf("after the wake the model is servable, got %q", reason)
		}
		// Nothing asleep any more: there is nothing to wake, and no call is made.
		if srv.wakeForRequest(ctx, "m") || calls.Load() != 1 {
			t.Fatal("an awake worker must not be resumed again")
		}
	})

	t.Run("an engine with no sleep mode is left alone", func(t *testing.T) {
		srv, st := newLeader(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNotImplemented)
			_, _ = w.Write([]byte(`{"status":"unsupported"}`))
		})
		if srv.wakeForRequest(ctx, "m") {
			t.Fatal("a 501 is not a wake")
		}
		if ps, _ := st.Placements().GetByNode(ctx, "w1"); ps[0].Status != PlacementSleeping {
			t.Fatalf("the placement must stay sleeping: %+v", ps)
		}
	})

	t.Run("past the budget the caller gets its 503, and the wake still lands", func(t *testing.T) {
		old := requestWakeBudget
		requestWakeBudget = 80 * time.Millisecond
		defer func() { requestWakeBudget = old }()
		srv, st := newLeader(t, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte(`{"status":"awake"}`))
		})
		if srv.wakeForRequest(ctx, "m") {
			t.Fatal("a wake slower than the budget must not hold the caller")
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if ps, _ := st.Placements().GetByNode(ctx, "w1"); len(ps) == 1 && ps[0].Status == "ready" {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatal("the resume was abandoned with the caller: the next request would start it again from nothing")
	})
}
