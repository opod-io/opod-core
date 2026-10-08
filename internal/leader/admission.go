package leader

// Admission is governed by worker SLOTS (ADR-091, feature "slot_admission";
// it amends ADR-082 §3 and ADR-086 §2).
//
// Every engine serves a fixed number of requests at once — llama.cpp's slots,
// vLLM's --max-num-seqs, SGLang's running-request limit — and queues the rest
// in its own first-come line, where nothing can rank them. Until ADR-091 the
// leader held a request only when NO worker could take it, so on a busy but
// working endpoint every request went straight into that engine queue and
// request classes ranked nothing: measured on the design-partner cell, a batch
// at 8 in flight against 4 llama.cpp slots pushed critical p95 TTFT from 22 ms
// to 535 ms. So the leader keeps the queue itself:
//
//   - each model has a POOL: the slots of the workers (or gangs) that take its
//     requests now, from what each worker reports (nodeapi.Heartbeat.Slots;
//     a gang's from its head), and the requests the gate has dispatched and
//     not yet seen complete. Dispatch happens only while in-flight < slots;
//   - a request that finds no slot waits in its class queue
//     (admission_classes.go). A COMPLETION frees a slot and the gate grants
//     exactly one waiter per freed slot, the highest eligible class first, by
//     flow inside it. A capacity change (a heartbeat, an undrain, a resume)
//     re-reads the pool and grants what it freed;
//   - each class may hold at most maxShare of the slots at once, so headroom is
//     reserved for the classes above it: a class at its share waits even while
//     slots are free;
//   - a pool with NO worker able to serve (slots 0) is ADR-082's case: the
//     request waits up to its class's holdMs for one, and holdMs 0 is the old
//     503 + Retry-After at once, with the reason `unavailable` gives;
//   - a pool that is merely BUSY holds a request up to its class's holdMs
//     when that is set, and with holdMs 0 has no deadline of the leader's: it
//     is the queue the engine used to keep, moved here where it can be ranked,
//     bounded by maxHeld and the client's own timeout;
//   - a worker that reports no slot count is UNGOVERNED: it makes its pool
//     unbounded, which is the pre-ADR-091 rule (hold only while nothing can
//     serve). /gatewayz lists such workers and a gauge counts them.
//
// One path, always: with no classes in the policy the same gate runs with one
// class, standard, share 1 (ADR-086 §4).
//
// Only chat completions pass the gate. /v1/embeddings and /v1/rerank reach
// the router directly, as they always have, and are not ranked; on a worker
// that serves both, their requests take engine slots the gate does not count.
//
// Everything is in memory and nothing on the request path reads the store:
// the pool's capacity is computed off the path, cached per model and
// invalidated by capacityChanged (and by age, for liveness that lapses
// without a write). No counter is kept per key and no token is counted
// (ADR-077 §5).

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/metrics"
)

// PolicyAdmission and PolicyAdmissionClass are the shared wire types
// (opod-sdk/adminapi).
type PolicyAdmission = adminapi.PolicyAdmission
type PolicyAdmissionClass = adminapi.PolicyAdmissionClass

// defaultMaxHeld is the cap when the policy sets no maxHeld. A held request
// costs an idle goroutine, its open connection and a body the gateway already
// capped; 64 of those is nothing to the leader. Since ADR-091 the cap also
// bounds the queue of a working endpoint past its slots — the queue that used
// to grow without bound inside the engine — so a burst larger than slots + 64
// is shed with 503 + Retry-After instead of queueing; an operator who wants a
// deeper queue raises maxHeld.
const defaultMaxHeld = 64

// capacityTTL bounds how long a pool's computed capacity is reused without a
// capacityChanged signal: liveness lapses by the clock (a heartbeat that stops
// writes nothing), and a pool must notice within about a second.
const capacityTTL = time.Second

type admissionState struct {
	// classes is the policy's admission half (classConfig); never nil once
	// a policy is applied. Before any policy: holdMs 0, one class.
	classes atomic.Pointer[classConfig]
	// held counts the requests waiting for a slot, every pool and class.
	held atomic.Int64
	gate slotGate
}

// set installs the policy's admission half.
func (a *admissionState) set(p PolicyAdmission) {
	a.classes.Store(newClassConfig(p))
}

// noPolicy is the admission half before any policy: one class, no budget.
var noPolicy = newClassConfig(PolicyAdmission{})

// config is the admission half in force: one atomic load.
func (a *admissionState) config() *classConfig {
	if c := a.classes.Load(); c != nil {
		return c
	}
	return noPolicy
}

// capView is what a pool can serve, computed off the request path.
type capView struct {
	slots      int    // governed slots across the workers that take requests now
	ungoverned int    // workers/gangs serving with no slot count: unbounded
	reason     string // non-empty = no worker can serve (the 503 text)
	gen        uint64 // the capacityChanged generation it was computed at
	at         time.Time
}

func (c capView) servable() bool { return c.reason == "" }
func (c capView) bounded() bool  { return c.ungoverned == 0 }

// pool is one model's admission state. Guarded by slotGate.mu.
type pool struct {
	model   string
	cap     capView
	inUse   int
	byClass [numClasses]int
	q       classQueues
	held    [numClasses]int64 // waiting, by class (the caps)
	all     int64
}

// slotGate holds every model's pool.
type slotGate struct {
	mu    sync.Mutex
	pools map[string]*pool
	gen   atomic.Uint64 // bumped by capacityChanged
}

// maxIdlePools bounds the pools map: a leader without a plan answers any
// model name a client sends, and each name would otherwise keep a pool.
const maxIdlePools = 1024

// pool returns model's pool, creating it. Called with the lock held.
func (g *slotGate) pool(model string) *pool {
	if g.pools == nil {
		g.pools = map[string]*pool{}
	}
	p, ok := g.pools[model]
	if !ok {
		if len(g.pools) >= maxIdlePools {
			for m, old := range g.pools {
				if old.inUse == 0 && old.all == 0 {
					delete(g.pools, m)
					metrics.ForgetAdmissionModel(m)
				}
			}
		}
		p = &pool{model: model}
		g.pools[model] = p
	}
	return p
}

// canGrant says whether a request of class may take a slot now. Called with
// the gate's lock held.
func (p *pool) canGrant(cfg *classConfig, class int) bool {
	if !p.cap.servable() {
		return false
	}
	if !p.cap.bounded() {
		return true // an ungoverned worker serves: no bound to keep (the pre-ADR-091 rule)
	}
	return p.inUse < p.cap.slots && p.byClass[class] < cfg.shareCap(class, p.cap.slots)
}

// take counts one granted request. Called with the gate's lock held.
func (p *pool) take(class int) {
	p.inUse++
	p.byClass[class]++
	metrics.SetAdmissionInUse(p.model, classNames[class], p.byClass[class])
}

// give returns one granted request's slot. Called with the gate's lock held.
func (p *pool) give(class int) {
	if p.byClass[class] > 0 {
		p.byClass[class]--
		p.inUse--
	}
	metrics.SetAdmissionInUse(p.model, classNames[class], p.byClass[class])
}

// dispatch grants waiters while slots are free: the highest eligible class
// first, by flow inside it — one grant per free slot. Called with the gate's
// lock held.
func (p *pool) dispatch(cfg *classConfig, a *admissionState) {
	for p.all > 0 {
		h := p.q.popEligible(func(class int) bool { return p.canGrant(cfg, class) })
		if h == nil {
			return
		}
		h.queued, h.granted = false, true
		p.uncount(h, a)
		p.take(h.class)
		close(h.done)
	}
}

// count holds h against the caps; uncount releases it. Called with the lock.
func (p *pool) count(h *heldReq, a *admissionState) {
	p.held[h.class]++
	p.all++
	a.held.Add(1)
	metrics.SetAdmissionHeld(classNames[h.class], p.held[h.class])
}

func (p *pool) uncount(h *heldReq, a *admissionState) {
	p.held[h.class]--
	p.all--
	a.held.Add(-1)
	metrics.SetAdmissionHeld(classNames[h.class], p.held[h.class])
}

// shed takes h out of the queue with a reason. Called with the lock held.
func (p *pool) shed(h *heldReq, why, reason string, a *admissionState) {
	p.q.remove(h)
	h.queued, h.shedWhy, h.reason = false, why, reason
	p.uncount(h, a)
	close(h.done)
}

// setCap installs a freshly computed capacity and acts on it: requests waiting
// with no budget of their class are shed when nothing can serve any more (a
// budget of 0 never waits for a worker, ADR-082 §2), and freed slots are
// granted. Called with the lock held.
func (p *pool) setCap(c capView, cfg *classConfig, a *admissionState) {
	if c.gen < p.cap.gen {
		return // a slower computation of an older generation
	}
	p.cap = c
	metrics.SetAdmissionCapacity(p.model, c.slots, c.ungoverned)
	if !c.servable() {
		for class := range p.q.q {
			if cfg.holdMs[class] > 0 {
				continue
			}
			for p.q.q[class].n > 0 {
				p.shed(p.q.q[class].newestOfLargest(), "unavailable", c.reason, a)
			}
		}
	}
	p.dispatch(cfg, a)
}

// capacityFor is model's capacity, recomputed when the generation moved or the
// cached view is older than capacityTTL. The computation reads the store and
// runs outside the gate's lock.
func (s *Server) capacityFor(ctx context.Context, model string) capView {
	g := &s.admission.gate
	gen := g.gen.Load()
	g.mu.Lock()
	cur := g.pool(model).cap
	g.mu.Unlock()
	if !cur.at.IsZero() && cur.gen == gen && time.Since(cur.at) < capacityTTL {
		return cur
	}
	c := s.computeCapacity(ctx, model)
	c.gen, c.at = gen, time.Now()
	g.mu.Lock()
	p := g.pool(model)
	p.setCap(c, s.admission.config(), &s.admission)
	c = p.cap // a newer generation may have landed first
	g.mu.Unlock()
	return c
}

// computeCapacity counts what model can serve: the slots of the workers and
// gangs that take its requests now, and those that report none. The 503
// reason, when nothing can, is unavailable's.
func (s *Server) computeCapacity(ctx context.Context, model string) capView {
	if why := s.unavailable(ctx, model); why != "" {
		return capView{reason: why}
	}
	slots, ungoverned := s.slotsOf(ctx, model)
	if slots == 0 && ungoverned == 0 {
		// Servable, yet no worker or gang counted: the leader's own engine
		// (or one only the router can judge). Not governed.
		ungoverned = 1
	}
	return capView{slots: slots, ungoverned: ungoverned}
}

// admit is the request path's gate. It returns a release to call when the
// request completes (the response is written — for a stream, its last byte),
// or the reason to shed it with; never both. started is when the request
// reached dispatch: a wake spends from the budget rather than adding to it.
func (s *Server) admit(ctx context.Context, model string, started time.Time) (release func(), reason string) {
	cfg := s.admission.config()
	class := cfg.classOf(admissionIdentFrom(ctx))
	cv := s.capacityFor(ctx, model)
	g := &s.admission.gate
	a := &s.admission

	g.mu.Lock()
	p := g.pool(model)
	// The fast path: nobody waits ahead and a slot of this class is free.
	if p.all == 0 && p.canGrant(cfg, class) {
		p.take(class)
		g.mu.Unlock()
		metrics.ObserveAdmissionWait(classNames[class], time.Since(started))
		return s.releaser(model, class), ""
	}
	budget := time.Duration(cfg.holdMs[class]) * time.Millisecond
	if !cv.servable() && budget <= 0 {
		g.mu.Unlock()
		return nil, cv.reason // ADR-082: 0 is the old 503 at once, nothing held
	}
	left := budget - time.Since(started)
	if budget > 0 && left <= 0 {
		g.mu.Unlock()
		return nil, s.shedOut(class, "deadline", s.busyReason(cv, model))
	}
	h := &heldReq{class: class, flow: admissionIdentFrom(ctx).flow, done: make(chan struct{})}
	if !p.enqueue(h, cfg, a) {
		g.mu.Unlock()
		return nil, s.shedOut(class, "cap", s.busyReason(cv, model))
	}
	p.dispatch(cfg, a) // a higher class may go ahead of the waiters at once
	if h.granted {
		g.mu.Unlock()
		metrics.ObserveAdmissionWait(classNames[class], time.Since(started))
		return s.releaser(model, class), "" // it never waited
	}
	g.mu.Unlock()

	var deadline <-chan time.Time
	if budget > 0 {
		t := time.NewTimer(left)
		defer t.Stop()
		deadline = t.C
	}
	select {
	case <-h.done:
	case <-deadline:
		if g.withdraw(model, h, a) {
			return nil, s.shedOut(class, "deadline", s.busyReason(s.capacityFor(ctx, model), model))
		}
	case <-ctx.Done():
		if g.withdraw(model, h, a) {
			metrics.ObserveAdmissionHold("cancelled")
			return nil, s.busyReason(cv, model)
		}
		if h.granted { // granted at the same moment: give it straight back
			s.releaser(model, class)()
		}
		metrics.ObserveAdmissionHold("cancelled")
		return nil, s.busyReason(cv, model)
	}
	// Left the queue by the gate's hand (the select may have raced it).
	switch {
	case h.granted:
		metrics.ObserveAdmissionHold("served")
		metrics.ObserveAdmissionSlotWait(classNames[class])
		metrics.ObserveAdmissionWait(classNames[class], time.Since(started))
		return s.releaser(model, class), ""
	case h.shedWhy == "unavailable":
		metrics.ObserveAdmissionHold("shed_deadline")
		metrics.ObserveAdmissionShed(classNames[class], "unavailable")
		return nil, h.reason
	default:
		return nil, s.shedOut(class, "evicted", s.busyReason(s.capacityFor(ctx, model), model))
	}
}

// enqueue puts h in its class's queue within the caps, shedding a request of a
// lower class when the hold is full. False = h itself is shed. Called with the
// gate's lock held.
func (p *pool) enqueue(h *heldReq, cfg *classConfig, a *admissionState) bool {
	if limit := cfg.maxHeld[h.class]; limit > 0 && p.held[h.class] >= limit {
		return false
	}
	if p.all >= cfg.total {
		v := p.q.victimBelow(h.class)
		if v == nil {
			return false
		}
		p.shed(v, "evicted", "", a)
	}
	h.queued = true
	p.q.push(h)
	p.count(h, a)
	return true
}

// withdraw takes a request that is still waiting out of its queue and reports
// true. False = the gate granted or shed it meanwhile, and the caller acts on
// that instead.
func (g *slotGate) withdraw(model string, h *heldReq, a *admissionState) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !h.queued {
		return false
	}
	p := g.pool(model)
	p.q.remove(h)
	h.queued = false
	p.uncount(h, a)
	return true
}

// releaser is the completion of one granted request: its slot goes back and,
// when someone waits, exactly one waiter per freed slot is granted. Safe to
// call once; later calls do nothing.
func (s *Server) releaser(model string, class int) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			g := &s.admission.gate
			g.mu.Lock()
			p := g.pool(model)
			p.give(class)
			p.dispatch(s.admission.config(), &s.admission)
			g.mu.Unlock()
		})
	}
}

// shedOut counts one shed and returns the reason to answer it with.
func (s *Server) shedOut(class int, why, reason string) string {
	metrics.ObserveAdmissionHold("shed_" + why)
	metrics.ObserveAdmissionShed(classNames[class], why)
	return reason
}

// busyReason is the 503 text for a request shed while waiting: the pool's own
// reason when nothing can serve, else that every slot it may use was busy.
func (s *Server) busyReason(cv capView, model string) string {
	if !cv.servable() {
		return cv.reason
	}
	return fmt.Sprintf("every worker slot for %s is busy (%d slot(s)); retry shortly", model, cv.slots)
}

// capacityChanged tells the gate that who-can-serve may have changed, in
// EITHER direction: every write that adds or removes a worker, a placement or
// a gang calls it (a heartbeat, a registration, a drain or undrain, a resume,
// a model load, unload, move or delete, a gang created or removed, a node
// removed). A reduction matters as much as a gain: a pool that kept admitting
// against a drained worker would send the request to a router with no worker
// for it, and the caller a 502 instead of the 503 + Retry-After. It bumps the
// generation — every cached view is stale from here — and re-reads the pools
// that have requests waiting, granting what the change freed. capacityTTL
// bounds what no write announces: liveness lapsing by the clock.
func (s *Server) capacityChanged() {
	g := &s.admission.gate
	g.gen.Add(1)
	g.mu.Lock()
	var waiting []string
	for m, p := range g.pools {
		if p.all > 0 {
			waiting = append(waiting, m)
		}
	}
	g.mu.Unlock()
	for _, m := range waiting {
		s.capacityFor(context.Background(), m)
	}
}

// admissionGovernance is what /gatewayz says about the gate: which models are
// governed by slots, and which workers are not (ADR-091).
func (s *Server) admissionGovernance() map[string]any {
	g := &s.admission.gate
	g.mu.Lock()
	models := map[string]any{}
	for m, p := range g.pools {
		models[m] = map[string]any{
			"slots":              p.cap.slots,
			"in_flight":          p.inUse,
			"held":               p.all,
			"ungoverned_workers": p.cap.ungoverned,
			"servable":           p.cap.servable(),
		}
	}
	g.mu.Unlock()
	return map[string]any{
		"governed_by":        "worker_slots",
		"models":             models,
		"ungoverned_workers": s.ungovernedWorkers(),
	}
}
