package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// drainedLeader is a router-only leader whose one worker holds model "m" and
// is drained, so every request for "m" finds no capacity — the case that was
// answered 503 + Retry-After at once before the admission hold.
func drainedLeader(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	cfg.Auth.RequireKeys = false
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", State: "ready", Address: "127.0.0.1:1", WorkerToken: "wtok", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat", strings.NewReader(`{"id":"w1","loaded_models":["m"]}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	if err := srv.DrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if srv.unavailable(ctx, "m") == "" {
		t.Fatal("a drained only holder must leave the model with no capacity")
	}
	return srv
}

// heldChat posts one chat request for "m" through the real gateway and reports
// the status, the Retry-After header and how long the answer took.
func heldChat(t *testing.T, srv *Server) (int, string, time.Duration) {
	t.Helper()
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	start := time.Now()
	resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header.Get("Retry-After"), time.Since(start)
}

// A budget of 0 is the old behaviour exactly: 503 + Retry-After at once, and
// nothing is held.
func TestAdmissionHoldZeroIsTodaysShed(t *testing.T) {
	srv := drainedLeader(t)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p0"})
	code, retry, took := heldChat(t, srv)
	if code != http.StatusServiceUnavailable || retry != "10" {
		t.Fatalf("no hold: want 503 + Retry-After 10, got %d %q", code, retry)
	}
	if took > 500*time.Millisecond {
		t.Fatalf("no hold must answer at once, took %s", took)
	}
	if n := srv.admission.held.Load(); n != 0 {
		t.Fatalf("nothing is held with a budget of 0: %d", n)
	}
}

// Capacity that comes back inside the budget serves the held request — on the
// signal, not on a clock: the undrain wakes it well before any heartbeat would.
func TestAdmissionHoldServesWhenCapacityReturns(t *testing.T) {
	srv := drainedLeader(t)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p1", Admission: PolicyAdmission{HoldMs: 5000}})
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = srv.UndrainNode(context.Background(), "w1")
	}()
	start := time.Now()
	why := srv.holdForCapacity(context.Background(), "m", "no capacity", start)
	took := time.Since(start)
	if why != "" {
		t.Fatalf("capacity came back inside the budget; the request must be served, got %q", why)
	}
	if took < 100*time.Millisecond || took > 2*time.Second {
		t.Fatalf("served on the undrain's signal, expected ~100ms, took %s", took)
	}
	if n := srv.admission.held.Load(); n != 0 {
		t.Fatalf("the slot is released once served: %d", n)
	}
}

// At the deadline the request is shed with exactly the old answer.
func TestAdmissionHoldShedsAtDeadline(t *testing.T) {
	srv := drainedLeader(t)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p1", Admission: PolicyAdmission{HoldMs: 200}})
	code, retry, took := heldChat(t, srv)
	if code != http.StatusServiceUnavailable || retry != "10" {
		t.Fatalf("at the deadline: want the old 503 + Retry-After 10, got %d %q", code, retry)
	}
	if took < 200*time.Millisecond || took > 2*time.Second {
		t.Fatalf("held for the budget and then shed, took %s", took)
	}
	if n := srv.admission.held.Load(); n != 0 {
		t.Fatalf("the slot is released at the deadline: %d", n)
	}
	// A budget already spent (say, by a wake) sheds without holding at all.
	if why := srv.holdForCapacity(context.Background(), "m", "x", time.Now().Add(-time.Second)); why == "" {
		t.Fatal("a spent budget cannot serve a model with no capacity")
	}
}

// maxHeld caps the waiters: past it a request is shed at once. A client that
// goes away releases its slot at once, and the next request may hold again.
func TestAdmissionHoldCapAndCancel(t *testing.T) {
	srv := drainedLeader(t)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p1", Admission: PolicyAdmission{HoldMs: 10000, MaxHeld: 1}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan string, 1)
	go func() { first <- srv.holdForCapacity(ctx, "m", "no capacity", time.Now()) }()
	deadline := time.Now().Add(2 * time.Second)
	for srv.admission.held.Load() != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first request never took a hold slot")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	if why := srv.holdForCapacity(context.Background(), "m", "no capacity", start); why == "" {
		t.Fatal("past the cap a request is shed, not served")
	}
	if took := time.Since(start); took > 200*time.Millisecond {
		t.Fatalf("past the cap a request is shed at once, took %s", took)
	}

	cancel()
	select {
	case why := <-first:
		if why == "" {
			t.Fatal("a cancelled hold must not report capacity")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a client going away must release its hold at once")
	}
	if n := srv.admission.held.Load(); n != 0 {
		t.Fatalf("the cancelled hold's slot is released: %d", n)
	}

	// maxHeld 0 means the default cap, not "no holds".
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p2", Admission: PolicyAdmission{HoldMs: 50}})
	if got := srv.admission.config().total; got != defaultMaxHeld {
		t.Fatalf("maxHeld 0 must mean the default cap, got %d", got)
	}
}

// The common way capacity returns is a heartbeat: a worker whose engine now
// reports the model. The heartbeat signals held requests once its rows are
// written, so the held request is served on it rather than at its deadline.
func TestAdmissionHoldServesOnHeartbeat(t *testing.T) {
	srv := drainedLeader(t)
	ctx := context.Background()
	_ = srv.UndrainNode(ctx, "w1")
	beat := func(models string) {
		rec := httptest.NewRecorder()
		srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat", strings.NewReader(`{"id":"w1","loaded_models":`+models+`}`)))
		if rec.Code != http.StatusOK {
			t.Errorf("heartbeat: %d %s", rec.Code, rec.Body.String())
		}
	}
	beat(`[]`) // the engine holds nothing: no capacity for "m"
	if srv.unavailable(ctx, "m") == "" {
		t.Fatal("a worker that reports nothing loaded is no capacity")
	}
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "p1", Admission: PolicyAdmission{HoldMs: 5000}})
	go func() {
		time.Sleep(100 * time.Millisecond)
		beat(`["m"]`)
	}()
	start := time.Now()
	if why := srv.holdForCapacity(ctx, "m", "no capacity", start); why != "" {
		t.Fatalf("the heartbeat brought the model back; want served, got %q", why)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("served on the heartbeat's signal, not the deadline: took %s", took)
	}
}
