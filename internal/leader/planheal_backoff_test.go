package leader

// A heal that failed must back off, and must say so on the event stream.
//
// Measured on the design-partner cell (2026-10-11): a gang the plan declared
// could not form (its coordinator died at start). The heal loop's create ran
// 16–50 s inside the ticker goroutine, so the next tick was already queued
// when it failed, and the next heal began a millisecond later. The per-gang
// create guard was held almost without a gap: every one of the manager's 21
// creates after the rollout was refused gang_create_in_flight in 0 ms, and the
// failures were only in the leader's log — the manager never read why.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/scheduler"
)

func TestHealBackoffSchedule(t *testing.T) {
	for n, want := range map[int]time.Duration{1: 15 * time.Second, 2: 30 * time.Second, 3: 60 * time.Second, 4: 60 * time.Second, 9: 60 * time.Second} {
		if got := healBackoffAfter(n); got != want {
			t.Errorf("after %d failed heals the wait is %s, want %s", n, got, want)
		}
	}
}

// The back-off holds a gang out of the pick until it ends, grows with each
// failure, and resets when a heal succeeds, when the gang is present again,
// and when the plan changes the gang's shape.
func TestHealBackoffHoldsAndResets(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	g := planGang{ID: "g1", Parts: 2, PP: 2, Flags: map[string]string{"ctx": "4194304"}}
	specs := []planGang{g}
	none := map[string]int{}
	h := newHealState()

	if got := h.admit(specs, none, now); len(got) != 1 {
		t.Fatalf("a gang that never failed is admitted: %v", got)
	}
	if wait := h.failedHeal(g, now); wait != 15*time.Second {
		t.Fatalf("first failure waits 15 s, got %s", wait)
	}
	if got := h.admit(specs, none, now.Add(14*time.Second)); len(got) != 0 {
		t.Fatalf("a gang inside its back-off must not be picked: %v", got)
	}
	if got := h.admit(specs, none, now.Add(15*time.Second)); len(got) != 1 {
		t.Fatalf("a gang whose back-off ended is admitted again: %v", got)
	}
	if wait := h.failedHeal(g, now.Add(15*time.Second)); wait != 30*time.Second {
		t.Fatalf("second failure in a row waits 30 s, got %s", wait)
	}

	// Success resets: the next failure starts from 15 s again.
	h.healed("g1")
	if got := h.admit(specs, none, now.Add(16*time.Second)); len(got) != 1 {
		t.Fatalf("a healed gang has no back-off: %v", got)
	}
	if wait := h.failedHeal(g, now.Add(20*time.Second)); wait != 15*time.Second {
		t.Fatalf("after a success the first failure waits 15 s again, got %s", wait)
	}

	// The plan changes the gang (a rollback restores a context that fits):
	// a new question, asked at once.
	fixed := planGang{ID: "g1", Parts: 2, PP: 2, Flags: map[string]string{"ctx": "8192"}}
	if got := h.admit([]planGang{fixed}, none, now.Add(21*time.Second)); len(got) != 1 {
		t.Fatalf("a gang whose shape changed in the plan must not inherit the old back-off: %v", got)
	}
	if _, ok := h.failed["g1"]; ok {
		t.Fatal("a changed shape must drop the back-off")
	}

	// The gang is present again (a manager formed it): back-off gone.
	h.failedHeal(g, now.Add(30*time.Second))
	h.admit(specs, map[string]int{"g1": 2}, now.Add(31*time.Second))
	if _, ok := h.failed["g1"]; ok {
		t.Fatal("a gang that is present again must drop its back-off")
	}
}

// The whole path on a real leader: the heal's create fails because its part's
// worker is replaced; the failure is an event with the code and the reason; the
// next pass does NOT start another create; a manager's create in that window
// gets the guard instead of gang_create_in_flight.
func TestAFailedHealBacksOffAndRecordsWhy(t *testing.T) {
	srv, _, w, entry := formationFixture(t)
	ctx := context.Background()
	srv.plan.mu.Lock()
	srv.plan.gangs = []planGang{{ID: "g1", Parts: 1}}
	srv.plan.modelID = entry.ID
	srv.plan.revision = 7
	srv.plan.present = true
	srv.plan.mu.Unlock()

	clock := time.Now()
	st := newHealState()
	st.now = func() time.Time { return clock }
	st.absent["g1"] = clock.Add(-2 * healAfter) // absent long enough to heal

	healed := make(chan struct{})
	go func() { srv.healGangs(ctx, st); close(healed) }()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the heal never started the gang's coordinator")
	}
	// The worker's pod is recreated while the gang forms: a definite failure.
	w.mu.Lock()
	w.gone = true
	w.mu.Unlock()
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "n1", BootID: "boot-b"}, Caller{Admin: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-healed:
	case <-time.After(10 * time.Second):
		t.Fatal("the heal's create did not end after its part's worker went away")
	}

	// The failure is on the event stream, with the code and the reason.
	evs, err := srv.store.EventLog().After(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var failed map[string]any
	for _, e := range evs {
		if e.Type == "shard.heal_failed" {
			if e.Subject != entry.ID {
				t.Errorf("the event's subject is the model, got %q", e.Subject)
			}
			failed = e.Data
		}
	}
	if failed == nil {
		t.Fatalf("a failed heal must record shard.heal_failed; events: %+v", evs)
	}
	if failed["gang"] != "g1" || failed["code"] != ShardCreatePartLost || failed["revision"] != float64(7) {
		t.Errorf("the event must carry gang, code %q and the plan revision: %v", ShardCreatePartLost, failed)
	}
	if r, _ := failed["reason"].(string); r == "" || !strings.Contains(r, "gang g1") || !strings.Contains(r, "n1") {
		t.Errorf("the event's reason must name the gang and the node: %q", failed["reason"])
	}

	// The next pass — the tick that was queued while the create ran — must
	// not start another create: the gang is backing off.
	w.mu.Lock()
	w.gone = false
	w.mu.Unlock()
	clock = clock.Add(time.Millisecond)
	second := make(chan struct{})
	go func() { srv.healGangs(ctx, st); close(second) }()
	select {
	case <-w.started:
		t.Fatal("a heal began right after the last one failed: the back-off did not hold")
	case <-second:
	case <-time.After(5 * time.Second):
		t.Fatal("the backed-off pass did not return")
	}

	// A manager's create in that window gets the guard.
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	mgr := make(chan error, 1)
	go func() {
		mgr <- srv.CreateShards(mctx, CreateShardsRequest{ModelID: entry.ID, Gang: "g1", Shards: 1, Nodes: []string{"n1"}})
	}()
	select {
	case err := <-mgr:
		if errors.Is(err, scheduler.ErrCreateInFlight) {
			t.Fatalf("a manager's create between heals was refused by the guard: %v", err)
		}
		t.Fatalf("the manager's create ended before starting its coordinator: %v", err)
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the manager's create never started its coordinator")
	}
	cancel()
	<-mgr
}
