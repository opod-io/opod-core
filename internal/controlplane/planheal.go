package controlplane

// Heal within the plan (§13 item 3), for gangs.
//
// A leader whose plan declares a gang can form that gang itself. Until this
// existed, forming one was only ever an admin call — POST /admin/v1/shards/create
// — so a gang that lost its parts came back only if something outside the
// leader noticed and asked again.
//
// What that cost, measured on the design-partner cell (2026-09-20, the P6
// drill): the control plane was scaled to zero, the autoscaler parked a
// two-part gang, and one request woke it. Kubernetes restored both pods in
// twelve seconds and both parts registered as ready workers — and the endpoint
// then answered "waking" to every request for as long as it was watched,
// because nobody was left to form the gang. The same endpoint, with the control
// plane running, recovered from the same state in 95 seconds. The autoscaler
// restores the WORKLOAD; without this loop nothing restores the GANG, and the
// headline promise of an autoscaler that survives a control-plane outage is
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
//   - it waits out healAfter first, so a control plane that is present and
//     doing its job always wins the race and this never fights it;
//   - it does not choose WHERE parts run. Placement stayed with whoever renders
//     the pods; by the time this loop runs, the parts are already on the nodes
//     that were approved for them.

import (
	"context"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// healAfter is how long a plan-declared gang must be completely absent before
// the leader forms it. Long enough that a control plane doing its job is never
// raced; short enough that an outage costs one window, not the outage.
const healAfter = 45 * time.Second

// healInterval is how often the loop looks.
const healInterval = 15 * time.Second

// planGang is a gang the plan declares, in the terms a create needs.
type planGang struct {
	ID      string
	Parts   int
	TP      int
	PP      int
	Devices int
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
	absent := map[string]time.Time{}
	go func() {
		t := time.NewTicker(healInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.healGangs(ctx, absent)
			}
		}
	}()
}

// healGangs forms any plan-declared gang that has been entirely absent for
// healAfter and has the workers to run on. absent carries each gang's clock
// between ticks.
func (s *Server) healGangs(ctx context.Context, absent map[string]time.Time) {
	specs := s.plan.gangSpecs()
	if len(specs) == 0 || s.orch == nil {
		return
	}
	_, model := s.plan.get()
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
	g, id, ok := pickGangToHeal(specs, held, free, absent, time.Now())
	if !ok {
		return
	}
	s.log.Info("healing within the plan: forming the gang the plan declares",
		"model", model, "gang", id, "parts", g.Parts, "free_workers", free)
	req := CreateShardsRequest{ModelID: model, Shards: g.Parts, Gang: g.ID, TP: g.TP, PP: g.PP, Devices: g.Devices}
	if err := s.CreateShards(ctx, req); err != nil {
		s.log.Warn("heal within the plan failed — will try again", "model", model, "gang", id, "err", err)
		return
	}
	delete(absent, id)
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
		id := g.ID
		if id == "" {
			id = store.DefaultGangID
		}
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
			continue // a control plane that is present forms it first
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
