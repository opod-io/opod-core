package leader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/router"
	"github.com/opod-io/opod/internal/store"
)

// admitOn asks the gate for a slot as one standard request and, once granted,
// records the worker the router sent it to — the trace the leader installs
// before the gate and the router fills (router.NoteNode).
func admitOn(t *testing.T, srv *Server, node string) func() {
	t.Helper()
	ctx := router.WithTrace(withAdmissionIdent(context.Background(), admissionIdent{class: classStandard, flow: "a"}))
	rel, why := srv.admit(ctx, "m", time.Now())
	if why != "" {
		t.Fatalf("a free slot must be granted at once, got %q", why)
	}
	router.NoteNode(ctx, node)
	return rel
}

// leaves is the heartbeat of a worker whose engine stopped: nothing loaded.
func leaves(t *testing.T, srv *Server, id string) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat",
		strings.NewReader(`{"id":"`+id+`","loaded_models":[],"engine":{"state":"stopped","detail":"signal: terminated"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
}

// returns is the heartbeat of that worker once its engine serves again.
func returns(t *testing.T, srv *Server, id string, slots int) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat",
		strings.NewReader(`{"id":"`+id+`","loaded_models":["m"],"slots":`+strconv.Itoa(slots)+`,"engine":{"state":"serving"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
}

// The drill-4b sequence (design-partner cell, 2026-10-12): two workers of 4
// slots each, 4 requests in flight on each and 4 more waiting. Worker A's
// engine is sent SIGTERM: it leaves the pool (slots 8 → 4) while its 4
// streams hang for 30 s. Those 4 took A's slots with them; they must not hold
// B's. When B's own 4 complete, the 4 waiting requests are granted on B at
// once — counted model-wide they waited for A's dying streams instead.
func TestSlotGateARequestOnADepartedWorkerHoldsNoSlotOfTheRest(t *testing.T) {
	srv := slottedLeader(t, 4, PolicyAdmission{HoldMs: 60000})
	ctx := context.Background()
	if err := srv.store.Nodes().Upsert(ctx, store.Node{ID: "w2", Hostname: "w2", State: "ready", Address: "127.0.0.1:2", WorkerToken: "wtok2", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	beat(t, srv, "w2", 4)
	var onA, onB []func()
	for i := 0; i < 4; i++ {
		onA = append(onA, admitOn(t, srv, "w1"))
		onB = append(onB, admitOn(t, srv, "w2"))
	}
	out := make(chan admitted, 4)
	for i := 0; i < 4; i++ {
		admitAs(srv, ctx, "waiting", classStandard, "a", out)
	}
	waitHeld(t, srv, 4)

	leaves(t, srv, "w1") // A's engine stopped: slots 8 → 4, its 4 streams still open
	none(t, out, "B's 4 slots are all in flight")
	gov := srv.admissionGovernance()["models"].(map[string]any)["m"].(map[string]any)
	if gov["slots"] != 4 || gov["stranded"] != 4 || gov["in_flight"] != 8 {
		t.Fatalf("after A left: want slots 4, in_flight 8, stranded 4; got %v", gov)
	}

	for _, rel := range onB {
		rel()
	}
	var granted []func()
	deadline := time.After(500 * time.Millisecond)
	for len(granted) < 4 {
		select {
		case a := <-out:
			if a.why != "" {
				t.Fatalf("a waiting request was shed: %q", a.why)
			}
			granted = append(granted, a.release)
		case <-deadline:
			t.Fatalf("B's slots are free but only %d of 4 waiting requests were granted: "+
				"the requests stranded on A still hold B's slots", len(granted))
		}
	}

	// A's streams end at last: the count comes back to what is really in
	// flight on B, with nothing negative or left over.
	for _, rel := range onA {
		rel()
	}
	if n := inUse(srv); n != 4 {
		t.Fatalf("in flight after A's streams ended: want 4 (on B), got %d", n)
	}
	// A back with its 4 slots: the pool admits exactly 8 again.
	returns(t, srv, "w1", 4)
	var more []func()
	for i := 0; i < 4; i++ {
		more = append(more, admitOn(t, srv, "w1"))
	}
	rel, why := srv.admit(withAdmissionIdent(ctx, admissionIdent{class: classStandard, flow: "b"}), "m", time.Now().Add(-time.Minute))
	if why == "" {
		rel()
		t.Fatal("with 8 in flight on 8 slots the next request must wait, not be granted")
	}
	for _, r := range append(granted, more...) {
		r()
	}
	if n := inUse(srv); n != 0 {
		t.Fatalf("every request ended: in flight %d", n)
	}
}

// A worker that leaves and comes back while its requests still run counts
// them again: they are on its slots, not stranded.
func TestSlotGateAReturningWorkerCountsItsRequestsAgain(t *testing.T) {
	srv := slottedLeader(t, 2, PolicyAdmission{HoldMs: 60000})
	r1 := admitOn(t, srv, "w1")
	r2 := admitOn(t, srv, "w1")
	leaves(t, srv, "w1")
	returns(t, srv, "w1", 2)
	out := make(chan admitted, 1)
	admitAs(srv, context.Background(), "third", classStandard, "a", out)
	waitHeld(t, srv, 1)
	none(t, out, "w1 is back with both its slots in flight")
	r1()
	if a := next(t, out); a.why != "" {
		t.Fatalf("the freed slot serves the waiting request, got %q", a.why)
	} else {
		a.release()
	}
	r2()
}
