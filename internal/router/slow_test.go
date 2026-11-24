package router

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/store"
)

// slowRouter records what the slow hook was told.
func slowRouter(t *testing.T, factor float64) (*Router, func() []SlowChange) {
	t.Helper()
	r := New(nil, nil)
	r.SetSlowFactor(factor)
	var mu sync.Mutex
	var got []SlowChange
	r.SetSlowHook(func(c SlowChange) { mu.Lock(); got = append(got, c); mu.Unlock() })
	return r, func() []SlowChange { mu.Lock(); defer mu.Unlock(); return append([]SlowChange(nil), got...) }
}

func penaltyOf(r *Router, model, key string) float64 {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.slowPenaltyLocked(model, key)
}

// The row's L1 proof (PLAN T28.12): a worker at 3× its sibling's TTFT is
// ranked down after 20 samples — not one sooner — and is cleared after 20
// normal ones; the hook is told each change once.
func TestAWorkerAtThreeTimesItsSiblingsTTFTIsRankedDownThenCleared(t *testing.T) {
	r, changes := slowRouter(t, 2)
	now := time.Now()
	const tokens = 1000
	for i := 0; i < slowWindow; i++ {
		r.noteServed("m", "fast", 100*time.Millisecond, tokens, 0, now)
	}
	for i := 0; i < slowWindow-1; i++ {
		r.noteServed("m", "slow", 300*time.Millisecond, tokens, 0, now)
	}
	if p := penaltyOf(r, "m", "slow"); p != 0 {
		t.Fatalf("penalty %v after %d samples, want none before %d", p, slowWindow-1, slowWindow)
	}
	r.noteServed("m", "slow", 300*time.Millisecond, tokens, 0, now)
	if p := penaltyOf(r, "m", "slow"); p < 2.99 || p > 3.01 {
		t.Fatalf("penalty %v after %d samples, want the factor 3", p, slowWindow)
	}
	if p := penaltyOf(r, "m", "fast"); p != 0 {
		t.Fatalf("the fast sibling has penalty %v", p)
	}
	ws := r.SlowWorkers()
	if len(ws) != 1 || ws[0].Key != "slow" || ws[0].Metric != SlowMetricTTFT || ws[0].Samples != slowWindow {
		t.Fatalf("slow workers %+v", ws)
	}

	// It ranks behind its sibling at equal load — never saturated by it.
	order, _ := r.rankCandidates(context.Background(), "m",
		[]candidate{workerCandidate("slow", false), workerCandidate("fast", false)}, nil)
	if order[0].key != "fast" {
		t.Fatalf("order %v, want the fast worker first", keysOf(order))
	}
	r.inflight["fast"] = 4 // busier by more than the factor: the slow worker serves
	order, _ = r.rankCandidates(context.Background(), "m",
		[]candidate{workerCandidate("slow", false), workerCandidate("fast", false)}, nil)
	if order[0].key != "slow" {
		t.Fatalf("order %v, want the slow worker once its sibling is busier by more than its factor", keysOf(order))
	}

	for i := 0; i < slowWindow; i++ {
		r.noteServed("m", "slow", 100*time.Millisecond, tokens, 0, now)
	}
	if p := penaltyOf(r, "m", "slow"); p != 0 {
		t.Fatalf("penalty %v after %d normal samples, want none", p, slowWindow)
	}
	if len(r.SlowWorkers()) != 0 {
		t.Fatalf("still listed: %+v", r.SlowWorkers())
	}
	got := changes()
	if len(got) != 2 || !got[0].Slow || got[0].Key != "slow" || got[1].Slow || got[1].Key != "slow" {
		t.Fatalf("hook told %+v, want one slow and one cleared for the slow worker", got)
	}
}

// The inter-token gap is judged the same way.
func TestASlowInterTokenGapIsRankedDown(t *testing.T) {
	r, _ := slowRouter(t, 2)
	now := time.Now()
	for i := 0; i < slowWindow; i++ {
		r.noteServed("m", "a", 0, -1, 10*time.Millisecond, now)
		r.noteServed("m", "b", 0, -1, 10*time.Millisecond, now)
		r.noteServed("m", "c", 0, -1, 50*time.Millisecond, now)
	}
	ws := r.SlowWorkers()
	if len(ws) != 1 || ws[0].Key != "c" || ws[0].Metric != SlowMetricGap || ws[0].Factor != 5 {
		t.Fatalf("slow workers %+v, want c at 5× on the gap", ws)
	}
}

// A model with one worker has nothing to compare with; nor does a worker
// whose sibling has fewer than 20 samples; nor anything with the factor off.
func TestNoSiblingNoPenalty(t *testing.T) {
	r, changes := slowRouter(t, 2)
	now := time.Now()
	for i := 0; i < 3*slowWindow; i++ {
		r.noteServed("m", "only", time.Second, 1000, time.Second, now)
	}
	for i := 0; i < slowWindow-1; i++ {
		r.noteServed("m2", "fast", time.Millisecond, 1000, 0, now)
	}
	for i := 0; i < slowWindow; i++ {
		r.noteServed("m2", "slow", time.Second, 1000, 0, now)
	}
	if ws := r.SlowWorkers(); len(ws) != 0 || len(changes()) != 0 {
		t.Fatalf("judged without a sibling's window: %+v", ws)
	}

	off, _ := slowRouter(t, 0)
	for i := 0; i < slowWindow; i++ {
		off.noteServed("m", "fast", time.Millisecond, 1000, 0, now)
		off.noteServed("m", "slow", time.Second, 1000, 0, now)
	}
	if len(off.SlowWorkers()) != 0 || off.slow.windows != nil {
		t.Fatal("slowFactor 0 recorded samples")
	}
}

// A slow worker passed over gathers no samples; once its own age out it is
// judged on nothing, is cleared and is tried again.
func TestAPenaltyEndsWhenItsSamplesAgeOut(t *testing.T) {
	r, changes := slowRouter(t, 2)
	then := time.Now()
	for i := 0; i < slowWindow; i++ {
		r.noteServed("m", "fast", 100*time.Millisecond, 1000, 0, then)
		r.noteServed("m", "slow", time.Second, 1000, 0, then)
	}
	if len(r.SlowWorkers()) != 1 {
		t.Fatalf("want slow ranked down first: %+v", r.SlowWorkers())
	}
	later := then.Add(slowSampleMaxAge + time.Second)
	r.noteServed("m", "fast", 100*time.Millisecond, 1000, 0, later)
	if p := penaltyOf(r, "m", "slow"); p != 0 || len(r.SlowWorkers()) != 0 {
		t.Fatalf("penalty %v after its samples aged out", p)
	}
	if got := changes(); len(got) != 2 || got[1].Slow {
		t.Fatalf("hook told %+v, want slow then cleared", got)
	}
}

// Turning the factor off clears every slow worker, and the hook is told, so
// no alert stays open; removing a node clears its verdicts too.
func TestTurningTheFactorOffClearsEverySlowWorker(t *testing.T) {
	r, changes := slowRouter(t, 2)
	now := time.Now()
	for i := 0; i < slowWindow; i++ {
		r.noteServed("m", "fast", 100*time.Millisecond, 1000, 0, now)
		r.noteServed("m", "slow", time.Second, 1000, 0, now)
		r.noteServed("m2", "fast", 100*time.Millisecond, 1000, 0, now)
		r.noteServed("m2", "slow", time.Second, 1000, 0, now)
	}
	r.InvalidateNode("slow")
	if len(r.SlowWorkers()) != 0 {
		t.Fatalf("a removed node is still listed: %+v", r.SlowWorkers())
	}
	for i := 0; i < slowWindow; i++ {
		r.noteServed("m", "slow", time.Second, 1000, 0, now)
	}
	r.SetSlowFactor(0)
	if len(r.SlowWorkers()) != 0 {
		t.Fatalf("still listed with the factor off: %+v", r.SlowWorkers())
	}
	slow, cleared := 0, 0
	for _, c := range changes() {
		if c.Slow {
			slow++
		} else {
			cleared++
		}
	}
	if slow != 3 || cleared != 3 {
		t.Fatalf("hook told %d slow and %d cleared, want 3 and 3: %+v", slow, cleared, changes())
	}
}

// Through the dispatch path: answers from a worker that takes 30× as long to
// its first event are timed by Chat and rank that worker down.
func TestChatTimesEachWorkersFirstEvent(t *testing.T) {
	r, _ := drainRouter(t,
		store.Node{ID: "w0", State: store.NodeStateReady},
		store.Node{ID: "w1", State: store.NodeStateReady})
	r.SetSlowFactor(2)
	answer := func(delay time.Duration) *scriptEngine {
		return &scriptEngine{chat: func(ctx context.Context, _ engines.ChatRequest) (<-chan engines.StreamEvent, error) {
			out := make(chan engines.StreamEvent, 3)
			go func() {
				defer close(out)
				time.Sleep(delay)
				out <- engines.StreamEvent{Delta: "a"}
				out <- engines.StreamEvent{Delta: "b"}
				out <- engines.StreamEvent{Done: true}
			}()
			return out, nil
		}}
	}
	r.remotes["w0"], r.remotes["w1"] = answer(30*time.Millisecond), answer(time.Millisecond)
	ctx := context.Background()
	for i := 0; i < slowWindow; i++ {
		for _, id := range []string{"w0", "w1"} {
			// Each request to the worker named: the other is made busier.
			other := map[string]string{"w0": "w1", "w1": "w0"}[id]
			r.mu.Lock()
			r.inflight[other] += 100
			r.mu.Unlock()
			s, err := r.Chat(ctx, promptOf(10))
			if err != nil {
				t.Fatal(err)
			}
			drainAll(s)
			r.mu.Lock()
			r.inflight[other] -= 100
			r.mu.Unlock()
		}
	}
	// The stream goroutine records after the last event: wait for it.
	deadline := time.Now().Add(2 * time.Second)
	for len(r.SlowWorkers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	ws := r.SlowWorkers()
	if len(ws) != 1 || ws[0].Key != "w0" || ws[0].Metric != SlowMetricTTFT {
		t.Fatalf("slow workers %+v, want w0 ranked down on its TTFT", ws)
	}
}

// A torn-down model's gangs leave the slow list, and their clear is sent:
// no alert stays open for a gang that is gone (InvalidateModel).
func TestATornDownGangIsNoLongerSlow(t *testing.T) {
	r, changes := slowRouter(t, 2)
	now := time.Now()
	g0, g1 := gangKey("m", "g0"), gangKey("m", "g1")
	for i := 0; i < slowWindow; i++ {
		r.noteServed("m", g0, 100*time.Millisecond, 1000, 0, now)
		r.noteServed("m", g1, time.Second, 1000, 0, now)
	}
	r.mu.Lock()
	r.prefill[g1] = 500
	r.mu.Unlock()
	if ws := r.SlowWorkers(); len(ws) != 1 || ws[0].Key != g1 {
		t.Fatalf("want g1 ranked down first: %+v", ws)
	}
	r.InvalidateModel("m")
	if ws := r.SlowWorkers(); len(ws) != 0 {
		t.Fatalf("a torn-down gang is still listed: %+v", ws)
	}
	if got := r.prefillTokens(); len(got) != 0 {
		t.Fatalf("a torn-down gang kept prefill tokens: %v", got)
	}
	if got := changes(); len(got) != 2 || got[1].Slow || got[1].Key != g1 {
		t.Fatalf("hook told %+v, want g1 slow then cleared", got)
	}
}
