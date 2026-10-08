package leader

import (
	"context"
	"fmt"
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

// slottedLeader is a router-only leader whose one worker serves model "m" and
// reports slots (0 = it reports none: ungoverned), under the given policy.
func slottedLeader(t *testing.T, slots int, adm PolicyAdmission) *Server {
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
	beat(t, srv, "w1", slots)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "s1", Admission: adm})
	return srv
}

// beat is one heartbeat from worker id holding "m" with the given slots.
func beat(t *testing.T, srv *Server, id string, slots int) {
	t.Helper()
	body := `{"id":"` + id + `","loaded_models":["m"]`
	if slots > 0 {
		body += fmt.Sprintf(`,"slots":%d`, slots)
	}
	rec := httptest.NewRecorder()
	srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat", strings.NewReader(body+"}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
}

// admitted is one request's outcome from the gate.
type admitted struct {
	name    string
	release func()
	why     string
}

// admitAs asks the gate for a slot as one request of class and flow, in the
// background, and reports the outcome on out.
func admitAs(srv *Server, ctx context.Context, name string, class int, flow string, out chan<- admitted) {
	go func() {
		rel, why := srv.admit(withAdmissionIdent(ctx, admissionIdent{class: class, flow: flow}), "m", time.Now())
		out <- admitted{name: name, release: rel, why: why}
	}()
}

// admitNow asks for a slot and expects it at once.
func admitNow(t *testing.T, srv *Server, class int, flow string) func() {
	t.Helper()
	rel, why := srv.admit(withAdmissionIdent(context.Background(), admissionIdent{class: class, flow: flow}), "m", time.Now())
	if why != "" {
		t.Fatalf("a free slot of class %s must be granted at once, got %q", classNames[class], why)
	}
	return rel
}

// next is the next outcome, or a failure after a while.
func next(t *testing.T, out <-chan admitted) admitted {
	t.Helper()
	select {
	case a := <-out:
		return a
	case <-time.After(3 * time.Second):
		t.Fatal("no request left the gate")
	}
	return admitted{}
}

// none asserts nobody leaves the gate for a short while.
func none(t *testing.T, out <-chan admitted, why string) {
	t.Helper()
	select {
	case a := <-out:
		t.Fatalf("%s: %s left the gate (%q)", why, a.name, a.why)
	case <-time.After(100 * time.Millisecond):
	}
}

func inUse(srv *Server) int {
	g := &srv.admission.gate
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pool("m").inUse
}

// A busy worker (requests in flight = its slots) holds the next request, on
// a working endpoint — the case the binary rule let straight through to the
// engine's own queue. A completion frees the slot for it.
func TestSlotGateBusyWorkerHoldsTheNextRequest(t *testing.T) {
	srv := slottedLeader(t, 2, PolicyAdmission{HoldMs: 5000})
	if srv.unavailable(context.Background(), "m") != "" {
		t.Fatal("the worker serves: this is a busy endpoint, not an unavailable one")
	}
	r1 := admitNow(t, srv, classStandard, "a")
	r2 := admitNow(t, srv, classStandard, "a")
	out := make(chan admitted, 1)
	admitAs(srv, context.Background(), "third", classStandard, "a", out)
	waitHeld(t, srv, 1)
	none(t, out, "both slots are in flight")
	if n := inUse(srv); n != 2 {
		t.Fatalf("in flight must stay at the slot count, have %d", n)
	}
	r1()
	a := next(t, out)
	if a.why != "" {
		t.Fatalf("a freed slot serves the held request, got %q", a.why)
	}
	if n := inUse(srv); n != 2 {
		t.Fatalf("the freed slot went to the held request: in flight %d", n)
	}
	r1() // a release is once only
	if n := inUse(srv); n != 2 {
		t.Fatalf("a second call of the same release must change nothing: in flight %d", n)
	}
	r2()
	a.release()
	if n := inUse(srv); n != 0 {
		t.Fatalf("every slot is back: in flight %d", n)
	}
}

// A completion releases EXACTLY ONE waiter per freed slot, and it is the
// highest class waiting — whatever order they arrived in.
func TestSlotGateCompletionReleasesOneCriticalFirst(t *testing.T) {
	srv := slottedLeader(t, 1, PolicyAdmission{HoldMs: 5000, Classes: []PolicyAdmissionClass{
		{Name: "critical", HoldMs: 5000}, {Name: "standard", HoldMs: 5000}, {Name: "sheddable", HoldMs: 5000},
	}})
	busy := admitNow(t, srv, classStandard, "x")
	out := make(chan admitted, 3)
	for i, w := range []struct {
		name  string
		class int
	}{{"sheddable", classSheddable}, {"standard", classStandard}, {"critical", classCritical}} {
		admitAs(srv, context.Background(), w.name, w.class, w.name, out)
		waitHeld(t, srv, int64(i+1))
	}
	release := busy
	for _, want := range []string{"critical", "standard", "sheddable"} {
		release()
		a := next(t, out)
		if a.name != want || a.why != "" {
			t.Fatalf("the freed slot must go to %s, went to %s (%q)", want, a.name, a.why)
		}
		none(t, out, "one completion frees one slot")
		release = a.release
	}
	release()
}

// The sheddable class cannot exceed its share while slots stay free, so a
// critical arrival finds a slot at once instead of waiting for a sheddable
// answer to finish (ADR-091 §2). Its default share is a quarter: one of four
// slots (measured on the design-partner cell, 2026-10-07: half cost critical
// p95 TTFT +114 % beside a batch, a quarter −4 %).
func TestSlotGateSheddableShareKeepsHeadroom(t *testing.T) {
	srv := slottedLeader(t, 4, PolicyAdmission{HoldMs: 5000, Classes: []PolicyAdmissionClass{
		{Name: "critical", HoldMs: 5000}, {Name: "sheddable", HoldMs: 5000},
	}})
	s1 := admitNow(t, srv, classSheddable, "batch")
	out := make(chan admitted, 1)
	admitAs(srv, context.Background(), "second sheddable", classSheddable, "batch", out)
	waitHeld(t, srv, 1)
	none(t, out, "sheddable is at its share (1 of 4) with three slots free")
	c1 := admitNow(t, srv, classCritical, "p")
	c2 := admitNow(t, srv, classCritical, "p")
	c3 := admitNow(t, srv, classCritical, "p")
	if n := inUse(srv); n != 4 {
		t.Fatalf("one sheddable and three critical in flight, have %d", n)
	}
	c1()
	none(t, out, "a critical completion frees a slot sheddable may not take at its share")
	s1()
	if a := next(t, out); a.why != "" {
		t.Fatalf("a sheddable completion lets the held sheddable go, got %q", a.why)
	} else {
		a.release()
	}
	c2()
	c3()

	// An explicit share overrides the default.
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "s2", Admission: PolicyAdmission{HoldMs: 5000, Classes: []PolicyAdmissionClass{
		{Name: "sheddable", HoldMs: 5000, MaxShare: 0.5},
	}}})
	first := admitNow(t, srv, classSheddable, "batch")
	second := admitNow(t, srv, classSheddable, "batch")
	admitAs(srv, context.Background(), "third sheddable", classSheddable, "batch", out)
	waitHeld(t, srv, 1)
	none(t, out, "a share of 0.5 of 4 slots is two")
	first()
	if a := next(t, out); a.why != "" {
		t.Fatalf("served when one of its two slots frees, got %q", a.why)
	} else {
		a.release()
	}
	second()
}

// Deficit round robin keeps working under saturation: with one slot, a flow
// with five requests waiting and a flow with two are served one for one until
// the small flow is done.
func TestSlotGateFairWithinAClassUnderSaturation(t *testing.T) {
	srv := slottedLeader(t, 1, PolicyAdmission{HoldMs: 5000})
	busy := admitNow(t, srv, classStandard, "x")
	out := make(chan admitted, 7)
	n := int64(0)
	for _, f := range []string{"big", "big", "big", "small", "big", "small", "big"} {
		n++
		admitAs(srv, context.Background(), f, classStandard, f, out)
		waitHeld(t, srv, n)
	}
	got := ""
	release := busy
	for range 7 {
		release()
		a := next(t, out)
		got += a.name[:1]
		release = a.release
	}
	release()
	if want := "bsbsbbb"; got != want {
		t.Fatalf("served %s, want %s", got, want)
	}
}

// No classes in the policy is one class, standard, share 1, on the same gate:
// a key's class is not ranked (ADR-086 §4), so a sheddable key may fill every
// slot, and the gate still holds past the slots.
func TestSlotGateNoClassesIsOneStandardClass(t *testing.T) {
	srv := slottedLeader(t, 2, PolicyAdmission{HoldMs: 5000})
	cfg := srv.admission.config()
	if cfg == nil || cfg.classed {
		t.Fatal("a policy without classes is the one-class gate, not a nil one")
	}
	if cfg.classOf(admissionIdent{class: classCritical}) != classStandard {
		t.Fatal("without classes every request is standard")
	}
	r1 := admitNow(t, srv, classSheddable, "batch")
	r2 := admitNow(t, srv, classSheddable, "batch") // share 1: no half-cap without classes
	out := make(chan admitted, 1)
	admitAs(srv, context.Background(), "critical key", classCritical, "p", out)
	waitHeld(t, srv, 1)
	none(t, out, "without classes a critical key is not ranked ahead: the slots are busy")
	r1()
	if a := next(t, out); a.why != "" {
		t.Fatalf("served on the completion, got %q", a.why)
	} else {
		a.release()
	}
	r2()
}

// A worker that reports no slot count is ungoverned: its pool is unbounded,
// which is the pre-ADR-091 rule — hold only when nothing can serve — and
// /gatewayz names it.
func TestSlotGateUngovernedWorkerIsUnbounded(t *testing.T) {
	srv := slottedLeader(t, 0, PolicyAdmission{HoldMs: 5000})
	var rels []func()
	for range 100 {
		rels = append(rels, admitNow(t, srv, classStandard, "a"))
	}
	if srv.admission.held.Load() != 0 {
		t.Fatal("an ungoverned pool holds nothing while a worker serves")
	}
	for _, r := range rels {
		r()
	}
	gov := srv.admissionGovernance()
	if ws, _ := gov["ungoverned_workers"].([]string); len(ws) != 1 || ws[0] != "w1" {
		t.Fatalf("/gatewayz must name the ungoverned worker, got %v", gov["ungoverned_workers"])
	}
	// The worker states its slots on its next heartbeat: governed from then on.
	beat(t, srv, "w1", 1)
	r := admitNow(t, srv, classStandard, "a")
	out := make(chan admitted, 1)
	admitAs(srv, context.Background(), "second", classStandard, "a", out)
	waitHeld(t, srv, 1)
	r()
	if a := next(t, out); a.why != "" {
		t.Fatalf("got %q", a.why)
	} else {
		a.release()
	}
	if ws, _ := srv.admissionGovernance()["ungoverned_workers"].([]string); len(ws) != 0 {
		t.Fatalf("a worker that reports slots is governed, still listed: %v", ws)
	}
}

// A budget of 0 is two rules (ADR-091): a busy pool waits with no deadline of
// the leader's — the engine's queue, moved here — while a pool with nothing
// able to serve sheds at once (ADR-082); a request waiting with no budget is
// shed with the unavailable reason when its workers stop serving.
func TestSlotGateZeroBudgetWaitsWhenBusyShedsWhenUnavailable(t *testing.T) {
	srv := slottedLeader(t, 1, PolicyAdmission{})
	busy := admitNow(t, srv, classStandard, "a")
	out := make(chan admitted, 2)
	admitAs(srv, context.Background(), "waits", classStandard, "a", out)
	waitHeld(t, srv, 1)
	none(t, out, "a busy pool with budget 0 waits, it does not shed")
	busy()
	a := next(t, out)
	if a.why != "" {
		t.Fatalf("served when the slot freed, got %q", a.why)
	}
	admitAs(srv, context.Background(), "then drained", classStandard, "a", out)
	waitHeld(t, srv, 1)
	if err := srv.DrainNode(context.Background(), "w1"); err != nil {
		t.Fatal(err)
	}
	b := next(t, out)
	if b.why == "" || !strings.Contains(b.why, "draining") {
		t.Fatalf("a no-budget wait is shed with the unavailable reason when nothing serves, got %q", b.why)
	}
	a.release()
	if srv.admission.held.Load() != 0 {
		t.Fatal("nothing is left held")
	}
}

// End to end through the HTTP path: a request granted a slot holds it until
// its answer is written, and returns it after — whatever the answer.
func TestSlotGateDispatchReturnsTheSlotOnCompletion(t *testing.T) {
	srv := slottedLeader(t, 1, PolicyAdmission{HoldMs: 5000})
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	for range 3 {
		resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if n := inUse(srv); n != 0 {
			t.Fatalf("the slot must be returned when the answer is written, in flight %d", n)
		}
	}
	if srv.admission.held.Load() != 0 {
		t.Fatal("sequential requests on a one-slot pool never wait")
	}
}

// The gate's queue is the endpoint's queue: a request waiting for a slot is
// counted in /loadz queue_depth, which autoscalers scale on — the engines'
// own queues read ~0 once the leader holds what is past their slots.
func TestSlotGateWaitersAreTheQueueDepth(t *testing.T) {
	srv := slottedLeader(t, 1, PolicyAdmission{HoldMs: 5000})
	busy := admitNow(t, srv, classStandard, "a")
	out := make(chan admitted, 2)
	admitAs(srv, context.Background(), "one", classStandard, "a", out)
	admitAs(srv, context.Background(), "two", classStandard, "a", out)
	waitHeld(t, srv, 2)
	if q := srv.workerLoad(context.Background(), time.Now()).QueueDepth; q != 2 {
		t.Fatalf("two requests wait for a slot: queue_depth must say 2, got %d", q)
	}
	busy()
	a := next(t, out)
	a.release()
	b := next(t, out)
	b.release()
	if q := srv.workerLoad(context.Background(), time.Now()).QueueDepth; q != 0 {
		t.Fatalf("nothing waits: queue_depth 0, got %d", q)
	}
}
