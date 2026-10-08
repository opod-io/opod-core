package leader

import (
	"context"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

func req(class int, flow string) *heldReq {
	return &heldReq{class: class, flow: flow, done: make(chan struct{})}
}

// Strict order between classes: every critical request leaves the queues
// before any standard one, and every standard one before any sheddable one,
// whatever order they arrived in.
func TestClassQueuesStrictOrder(t *testing.T) {
	var q classQueues
	arrivals := []int{classSheddable, classStandard, classCritical, classSheddable, classCritical, classStandard, classSheddable, classCritical}
	for i, c := range arrivals {
		q.push(req(c, fmt.Sprintf("f%d", i%3)))
	}
	last := classCritical
	for range arrivals {
		h := q.pop()
		if h == nil {
			t.Fatal("the queues emptied early")
		}
		if h.class < last {
			t.Fatalf("a %s request left after a %s one", classNames[h.class], classNames[last])
		}
		last = h.class
	}
	if q.pop() != nil || q.total() != 0 {
		t.Fatal("the queues must be empty once every request has left")
	}
}

// Deficit round robin, as a property over random arrivals and departures:
// while a flow has a request waiting, no other flow of its class is
// served twice. That is "no flow starves another of its class" (ADR-086 §2)
// stated so a test can check it: a flow sending a thousand requests and a flow
// sending one are served one for one.
func TestClassQueuesDRRStarvesNoFlow(t *testing.T) {
	for seed := int64(1); seed <= 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		var q drrQueue
		flows := []string{"a", "b", "c", "d", "e"}
		waiting := map[string]int{}
		// servedSince[f][g] counts g's services since f last had nothing
		// waiting or was itself served.
		servedSince := map[string]map[string]int{}
		var all []*heldReq
		for step := 0; step < 400; step++ {
			switch op := rng.Intn(10); {
			case op < 5: // arrival; one flow sends far more than the rest
				f := flows[rng.Intn(len(flows))]
				if rng.Intn(2) == 0 {
					f = "a"
				}
				h := req(classStandard, f)
				q.push(h)
				all = append(all, h)
				if waiting[f] == 0 {
					servedSince[f] = map[string]int{}
				}
				waiting[f]++
			case op < 9: // a service
				h := q.pop()
				if h == nil {
					continue
				}
				waiting[h.flow]--
				delete(servedSince, h.flow)
				if waiting[h.flow] > 0 {
					servedSince[h.flow] = map[string]int{}
				}
				for f, by := range servedSince {
					by[h.flow]++
					if by[h.flow] > 1 {
						t.Fatalf("seed %d step %d: flow %s served twice while flow %s waited", seed, step, h.flow, f)
					}
				}
			default: // a client goes away
				if len(all) == 0 {
					continue
				}
				h := all[rng.Intn(len(all))]
				if q.remove(h) {
					waiting[h.flow]--
					if waiting[h.flow] == 0 {
						delete(servedSince, h.flow)
					}
				}
			}
			n := 0
			for _, w := range waiting {
				n += w
			}
			if n != q.n {
				t.Fatalf("seed %d step %d: the queue counts %d waiting, the test %d", seed, step, q.n, n)
			}
		}
	}
}

// Two flows, one of them flooding: they alternate until the small one is done.
func TestClassQueuesDRRAlternates(t *testing.T) {
	var q drrQueue
	for i := 0; i < 10; i++ {
		q.push(req(classStandard, "big"))
	}
	q.push(req(classStandard, "small"))
	q.push(req(classStandard, "small"))
	got := ""
	for h := q.pop(); h != nil; h = q.pop() {
		got += h.flow[:1]
	}
	if want := "bsbsbbbbbbbb"; got != want {
		t.Fatalf("served %s, want %s", got, want)
	}
}

// A request may lower its own class, never raise it; a key with no class, or
// a name that is not a class, is standard.
func TestEffectiveClassOnlyLowers(t *testing.T) {
	for _, c := range []struct {
		key, header string
		want        int
	}{
		{"", "", classStandard},
		{"bogus", "", classStandard},
		{"critical", "", classCritical},
		{"critical", "sheddable", classSheddable}, // lowered: honoured
		{"critical", "Standard ", classStandard},  // case and space do not matter
		{"standard", "critical", classStandard},   // raised: ignored
		{"sheddable", "critical", classSheddable}, // raised: ignored
		{"sheddable", "standard", classSheddable}, // raised: ignored
		{"standard", "bogus", classStandard},
		{"", "sheddable", classSheddable},
	} {
		if got := effectiveClass(c.key, c.header); got != c.want {
			t.Errorf("key %q header %q: got %s, want %s", c.key, c.header, classNames[got], classNames[c.want])
		}
	}
}

// The class comes from the key in the auth snapshot, the flow from its team,
// and the header may lower the class but not raise it.
func TestRequestIdentFromSnapshotKey(t *testing.T) {
	srv := drainedLeader(t)
	ctx := context.Background()
	err := srv.applyAuthSnapshot(ctx, &AuthSnapshot{Revision: "a1", Keys: []SnapshotKey{
		{ID: "k_crit", Name: "prod", Hash: "h-crit", Class: "critical", Team: "search"},
		{ID: "k_std", Name: "dev", Hash: "h-std"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ident := func(keyID, header string) admissionIdent {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if header != "" {
			r.Header.Set(classHeader, header)
		}
		if keyID != "" {
			r = r.WithContext(auth.WithTestKey(r.Context(), &store.APIKey{ID: keyID}))
		}
		return srv.requestIdent(r)
	}
	if got := ident("k_cp_k_crit", ""); got.class != classCritical || got.flow != "team:search" {
		t.Fatalf("a critical key of team search: got %+v", got)
	}
	if got := ident("k_cp_k_crit", "sheddable"); got.class != classSheddable {
		t.Fatalf("lowered by the header: got %s", classNames[got.class])
	}
	if got := ident("k_cp_k_std", "critical"); got.class != classStandard || got.flow != "key:k_cp_k_std" {
		t.Fatalf("a standard key asking for critical stays standard, its own flow: got %+v", got)
	}
	if got := ident("", "critical"); got.class != classStandard || got.flow != "" {
		t.Fatalf("no key: the anonymous standard flow, got %+v", got)
	}
}

// classedLeader is drainedLeader with request classes in its policy.
func classedLeader(t *testing.T, adm PolicyAdmission) *Server {
	srv := drainedLeader(t)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "c1", Admission: adm})
	if srv.admission.classes.Load() == nil {
		t.Fatal("a policy with classes must turn the classed hold on")
	}
	return srv
}

func holdAs(srv *Server, ctx context.Context, class int, flow string) string {
	return srv.holdForCapacity(withAdmissionIdent(ctx, admissionIdent{class: class, flow: flow}), "m", "no capacity", time.Now())
}

// End to end through the classed hold: requests of every class wait, and when
// capacity returns every one is served on the signal.
func TestClassedHoldServesEveryClassWhenCapacityReturns(t *testing.T) {
	srv := classedLeader(t, PolicyAdmission{HoldMs: 5000, Classes: []PolicyAdmissionClass{
		{Name: "critical", HoldMs: 5000}, {Name: "sheddable", HoldMs: 5000},
	}})
	results := make(chan string, 9)
	for i := 0; i < 9; i++ {
		go func(i int) { results <- holdAs(srv, context.Background(), i%numClasses, fmt.Sprintf("f%d", i%2)) }(i)
	}
	waitHeld(t, srv, 9)
	_ = srv.UndrainNode(context.Background(), "w1")
	for i := 0; i < 9; i++ {
		select {
		case why := <-results:
			if why != "" {
				t.Fatalf("capacity came back; every class must be served, got %q", why)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("a held request was not released when capacity returned")
		}
	}
	if n := srv.admission.held.Load(); n != 0 {
		t.Fatalf("every slot is released: %d", n)
	}
}

func waitHeld(t *testing.T, srv *Server, n int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for srv.admission.held.Load() != n {
		if time.Now().After(deadline) {
			t.Fatalf("want %d held, have %d", n, srv.admission.held.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Each class has its own budget: a sheddable request is shed at its short
// budget while a critical one goes on waiting, and a class not listed takes
// the admission-wide budget.
func TestClassedHoldPerClassBudget(t *testing.T) {
	srv := classedLeader(t, PolicyAdmission{HoldMs: 0, Classes: []PolicyAdmissionClass{
		{Name: "critical", HoldMs: 5000}, {Name: "sheddable", HoldMs: 100},
	}})
	critical := make(chan string, 1)
	go func() { critical <- holdAs(srv, context.Background(), classCritical, "p") }()
	start := time.Now()
	if why := holdAs(srv, context.Background(), classSheddable, "b"); why == "" {
		t.Fatal("a sheddable request past its budget is shed")
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > 2*time.Second {
		t.Fatalf("shed at the sheddable budget, took %s", took)
	}
	// standard is not listed and the admission-wide budget is 0: shed at once.
	start = time.Now()
	if why := holdAs(srv, context.Background(), classStandard, "s"); why == "" || time.Since(start) > 200*time.Millisecond {
		t.Fatal("an unlisted class takes the admission-wide budget, here 0: shed at once")
	}
	select {
	case <-critical:
		t.Fatal("the critical request is still inside its budget")
	default:
	}
	_ = srv.UndrainNode(context.Background(), "w1")
	if why := <-critical; why != "" {
		t.Fatalf("the critical request is served when capacity returns, got %q", why)
	}
}

// A full hold sheds the lowest class first: a critical request arriving at
// the cap takes the place of a sheddable one, while a sheddable request
// arriving at the cap is shed itself. A class's own cap sheds its arrival.
func TestClassedHoldOverflowShedsLowestClass(t *testing.T) {
	srv := classedLeader(t, PolicyAdmission{HoldMs: 5000, MaxHeld: 2, Classes: []PolicyAdmissionClass{
		{Name: "critical", HoldMs: 5000, MaxHeld: 1}, {Name: "sheddable", HoldMs: 5000},
	}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shed := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func(i int) { shed <- holdAs(srv, ctx, classSheddable, fmt.Sprintf("b%d", i)) }(i)
	}
	waitHeld(t, srv, 2)
	start := time.Now()
	if why := holdAs(srv, ctx, classSheddable, "b9"); why == "" || time.Since(start) > 200*time.Millisecond {
		t.Fatal("at the cap with nothing lower held, a sheddable arrival is shed at once")
	}
	critical := make(chan string, 1)
	go func() { critical <- holdAs(srv, ctx, classCritical, "p") }()
	select {
	case why := <-shed:
		if why == "" {
			t.Fatal("the evicted sheddable request must be shed, not served")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a critical arrival at the cap must shed a sheddable request at once")
	}
	waitHeld(t, srv, 2)
	if why := holdAs(srv, ctx, classCritical, "p2"); why == "" {
		t.Fatal("past the critical class's own cap a critical arrival is shed")
	}
	_ = srv.UndrainNode(context.Background(), "w1")
	if why := <-critical; why != "" {
		t.Fatalf("the critical request that took the place is served, got %q", why)
	}
	if why := <-shed; why != "" {
		t.Fatalf("the sheddable request that kept its place is served, got %q", why)
	}
}

// A client that goes away leaves the queues at once, and the request that
// remains is still served — the turn is not lost with it.
func TestClassedHoldCancelKeepsTheTurnMoving(t *testing.T) {
	srv := classedLeader(t, PolicyAdmission{HoldMs: 5000, Classes: []PolicyAdmissionClass{{Name: "critical", HoldMs: 5000}}})
	ctx, cancel := context.WithCancel(context.Background())
	gone := make(chan string, 1)
	go func() { gone <- holdAs(srv, ctx, classCritical, "a") }()
	waitHeld(t, srv, 1)
	stays := make(chan string, 1)
	go func() { stays <- holdAs(srv, context.Background(), classStandard, "b") }()
	waitHeld(t, srv, 2)
	cancel()
	if why := <-gone; why == "" {
		t.Fatal("a cancelled hold must not report capacity")
	}
	waitHeld(t, srv, 1)
	_ = srv.UndrainNode(context.Background(), "w1")
	select {
	case why := <-stays:
		if why != "" {
			t.Fatalf("the remaining request is served, got %q", why)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the remaining request was never given the turn")
	}
}
