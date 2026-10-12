package leader

// Heal within the plan (§13 item 3), for gangs.
//
// A leader whose plan declares a gang can form that gang itself. Until this
// existed, forming one was only ever an admin call — POST /admin/v1/shards/create
// — so a gang that lost its parts came back only if something outside the
// leader noticed and asked again.
//
// What that cost, measured on the design-partner cell (2026-09-20, the P6
// drill): the manager was scaled to zero, the autoscaler parked a
// two-part gang, and one request woke it. Kubernetes restored both pods in
// twelve seconds and both parts registered as ready workers — and the endpoint
// then answered "waking" to every request for as long as it was watched,
// because nobody was left to form the gang. The same endpoint, with the
// manager running, recovered from the same state in 95 seconds. The autoscaler
// restores the WORKLOAD; without this loop nothing restores the GANG, and the
// headline promise of an autoscaler that survives a manager outage is
// false for every sharded endpoint.
//
// The leader already had everything it needed: the plan mounted at
// /etc/opod/plan.json carries the gang's id, its parts, its parallelism. It was
// read for the model id and the revision and otherwise ignored.
//
// The rule is deliberately narrow — this loop heals, it does not place:
//
//   - it acts only on a gang the PLAN declares, never one it invents;
//   - it acts only when that gang has NO parts at all. A gang missing one part
//     is a different problem (the part comes back, or the plan is re-applied);
//     re-creating the whole gang would take down the parts still serving;
//   - it acts only when enough free workers have registered, which is exactly
//     the state an autoscaler leaves behind when it wakes a parked gang;
//   - it waits out healAfter first, so a manager that is present and
//     doing its job wins the first race and this never fights it;
//   - after a heal that FAILED it backs off that gang — 15 s, then 30 s, then
//     60 s at most — so the manager wins every race after the first too. A
//     create holds the gang's guard for as long as it runs (16–50 s for a
//     coordinator that dies at start), the ticker had the next pass queued by
//     the time it returned, and without the back-off the next heal began a
//     millisecond after the last failed: on the design-partner cell
//     (2026-10-11) every one of a manager's 21 creates after a rollout was
//     refused gang_create_in_flight. The back-off resets when a heal
//     succeeds, when the gang is present again, and when the plan changes
//     that gang's shape (a rollout or a rollback is a new question);
//   - a heal that fails says so on the event stream (shard.heal_failed, with
//     the create's error.code and its reason), not only in the log — a
//     manager that cannot form the gang itself, because this loop holds the
//     guard, reads the leader's own verdict there;
//   - it does not choose WHERE parts run. Placement stayed with whoever renders
//     the pods; by the time this loop runs, the parts are already on the nodes
//     that were approved for them.

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// healAfter is how long a plan-declared gang must be completely absent before
// the leader forms it. Long enough that a manager doing its job is never
// raced; short enough that an outage costs one window, not the outage.
const healAfter = 45 * time.Second

// healInterval is how often the loop looks.
const healInterval = 15 * time.Second

// After a heal fails, the same gang is not tried again for healBackoffBase,
// doubling on each further failure up to healBackoffMax. The base is one
// healInterval: the pass the ticker queued while the failed create ran is
// skipped, and a manager retrying every few seconds gets the guard in between.
const (
	healBackoffBase = 15 * time.Second
	healBackoffMax  = 60 * time.Second
)

// healBackoffAfter is the wait after the n-th consecutive failed heal of a
// gang (n ≥ 1): 15 s, 30 s, 60 s, 60 s, …
func healBackoffAfter(n int) time.Duration {
	d := healBackoffBase
	for i := 1; i < n && d < healBackoffMax; i++ {
		d *= 2
	}
	return min(d, healBackoffMax)
}

// healState is what the loop carries between passes: each gang's absence
// clock and, for a gang whose heal failed, its back-off.
type healState struct {
	absent map[string]time.Time     // gang id → first pass it was seen with no parts
	failed map[string]healBackoffAt // gang id → back-off after a failed heal
	now    func() time.Time
}

// healBackoffAt is one gang's back-off: the shape that failed, how many times
// in a row, and the earliest time it may be tried again.
type healBackoffAt struct {
	spec      planGang
	failures  int
	notBefore time.Time
}

func newHealState() *healState {
	return &healState{absent: map[string]time.Time{}, failed: map[string]healBackoffAt{}, now: time.Now}
}

// admit drops the gangs still backing off after a failed heal. A gang that is
// present again, or whose shape in the plan is no longer the one that failed,
// loses its back-off here.
func (h *healState) admit(specs []planGang, held map[string]int, now time.Time) []planGang {
	out := make([]planGang, 0, len(specs))
	for _, g := range specs {
		id := gangID(g)
		if b, ok := h.failed[id]; ok {
			switch {
			case held[id] > 0 || !reflect.DeepEqual(b.spec, g):
				delete(h.failed, id)
			case now.Before(b.notBefore):
				continue
			}
		}
		out = append(out, g)
	}
	return out
}

// failedHeal records one more failed heal of the gang and returns how long it
// now waits.
func (h *healState) failedHeal(g planGang, now time.Time) time.Duration {
	id := gangID(g)
	b := h.failed[id]
	if !reflect.DeepEqual(b.spec, g) {
		b = healBackoffAt{spec: g}
	}
	b.failures++
	wait := healBackoffAfter(b.failures)
	b.notBefore = now.Add(wait)
	h.failed[id] = b
	return wait
}

// healed forgets the gang: it formed.
func (h *healState) healed(id string) {
	delete(h.absent, id)
	delete(h.failed, id)
}

// gangID is the gang's id as the store records it.
func gangID(g planGang) string {
	if g.ID == "" {
		return store.DefaultGangID
	}
	return g.ID
}

// planGang is a gang the plan declares, in the terms a create needs.
type planGang struct {
	ID      string
	Parts   int
	TP      int
	PP      int
	Devices int
	Flags   map[string]string // the gang's engine knobs, reaching the coordinator (T14.26)
	Expert  bool              // an expert-parallel gang (shard_expert)
}

// gangs copies out the gangs the plan declares.
func (p *planFileState) gangSpecs() []planGang {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]planGang(nil), p.gangs...)
}

// StartGangHealer runs the heal-within-plan loop for gangs. No plan file means
// no plan to heal within, and the loop never starts.
func (s *Server) StartGangHealer(ctx context.Context) {
	st := newHealState()
	go func() {
		t := time.NewTicker(healInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.healGangs(ctx, st)
			}
		}
	}()
}

// healGangs forms any plan-declared gang that has been entirely absent for
// healAfter, has the workers to run on and is not backing off after a failed
// heal. st carries each gang's clocks between ticks.
func (s *Server) healGangs(ctx context.Context, st *healState) {
	specs := s.plan.gangSpecs()
	if len(specs) == 0 || s.orch == nil {
		return
	}
	revision, model := s.plan.get()
	if model == "" || models.FindByID(s.cat, model) == nil {
		return
	}
	shards, err := s.store.Shards().GetByModel(ctx, model)
	if err != nil {
		return // the store is the truth here; a read that failed proves nothing
	}
	held := map[string]int{} // gang id → parts on record
	for _, sh := range shards {
		held[sh.Gang()]++
	}
	free := len(s.freeWorkers(ctx, shards))
	g, id, ok := pickGangToHeal(st.admit(specs, held, st.now()), held, free, st.absent, st.now())
	if !ok {
		return
	}
	s.log.Info("healing within the plan: forming the gang the plan declares",
		"model", model, "gang", id, "parts", g.Parts, "free_workers", free)
	req := CreateShardsRequest{ModelID: model, Shards: g.Parts, Gang: g.ID, TP: g.TP, PP: g.PP, Devices: g.Devices, Flags: g.Flags, Expert: g.Expert}
	if err := s.CreateShards(ctx, req); err != nil {
		if errors.Is(err, scheduler.ErrCreateInFlight) {
			// Somebody is already forming it — a manager doing its job,
			// which this loop exists to stand in for, not to race.
			s.log.Info("a create is already forming this gang — leaving it to finish", "model", model, "gang", id)
			return
		}
		if ctx.Err() != nil {
			return // the leader is shutting down; that is not the gang's failure
		}
		wait := st.failedHeal(g, st.now())
		code, reason := shardCreateFailure(err)
		s.log.Warn("heal within the plan failed — backing off before trying again",
			"model", model, "gang", id, "code", code, "retry_in", wait, "err", err)
		s.record("shard.heal_failed", model, map[string]any{
			"gang": id, "parts": g.Parts, "by": "plan", "revision": revision,
			"code": code, "reason": reason, "retry_in_s": int(wait / time.Second),
		})
		return
	}
	st.healed(id)
	s.record("shard.healed", model, map[string]any{"gang": id, "parts": g.Parts, "by": "plan"})
}

// pickGangToHeal is the whole decision: which plan-declared gang, if any, this
// leader should form right now. absent carries each gang's absence clock and is
// updated here — a gang seen absent for the first time starts its clock and is
// not formed on that pass.
//
// At most one gang per pass, because forming one changes who is free.
func pickGangToHeal(specs []planGang, held map[string]int, free int, absent map[string]time.Time, now time.Time) (planGang, string, bool) {
	for _, g := range specs {
		id := gangID(g)
		if held[id] > 0 {
			// Present, in whole or in PART. A gang missing one part is not
			// this loop's business: re-creating it would take down the parts
			// still serving.
			delete(absent, id)
			continue
		}
		since, seen := absent[id]
		if !seen {
			absent[id] = now
			continue
		}
		if now.Sub(since) < healAfter {
			continue // a manager that is present forms it first
		}
		if free < g.Parts {
			continue // the parts are not here yet; nothing to form a gang over
		}
		return g, id, true
	}
	return planGang{}, "", false
}

// freeWorkers counts the workers that could take a part right now: alive, not
// drained, and not already holding a part of some gang. The leader itself is
// never one (D4: a managed gang's coordinator is a worker rank).
func (s *Server) freeWorkers(ctx context.Context, shards []store.Shard) []store.Node {
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return nil
	}
	taken := map[string]bool{}
	for _, sh := range shards {
		taken[sh.NodeID] = true
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	var out []store.Node
	for _, n := range nodes {
		if n.ID == "local" || taken[n.ID] || !n.TakesNewWork(maxAge, now) {
			continue
		}
		out = append(out, n)
	}
	return out
}
