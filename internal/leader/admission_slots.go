package leader

// Where a pool's slots come from (ADR-091).
//
//   - a worker states its engine's slot count on every heartbeat
//     (nodeapi.Heartbeat.Slots): the engine's own word where it gives one
//     (llama.cpp /props total_slots, SGLang's effective running-request
//     limit), else the number on the line the worker launched the engine with
//     (vLLM --max-num-seqs, which the worker always sets). A heartbeat with
//     none clears it: that worker is UNGOVERNED.
//   - a gang's slots live on its HEAD: the coordinator process the router
//     dials holds the requests. The leader asks the coordinator itself
//     (engines.SlotReporter, read once per coordinator, off the request path)
//     and falls back to the head worker's heartbeat.
//   - a door (`--role gateway`) learns each worker's slots from the leader's
//     registry, which it mirrors (gateway.Mirror), and counts only its own
//     dispatches against them.
//
// Every number here is in memory: a sync.Map written on the heartbeat and
// read by the router's picker and the gate's capacity computation.

import (
	"context"
	"sort"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/router"
	"github.com/opod-io/opod/internal/store"
)

// noteSlots records what a worker's heartbeat (or a door's mirror) says about
// its slots; n ≤ 0 = it cannot say.
func (s *Server) noteSlots(nodeID string, n int) {
	metrics.SetWorkerSlots(nodeID, n)
	if n <= 0 {
		s.slots.Delete(nodeID)
		return
	}
	s.slots.Store(nodeID, n)
}

// slotsOfKey is the slot count the router reads for a worker (its node id) or
// a gang (router.GangKey): ok = false when it reports none.
func (s *Server) slotsOfKey(key string) (int, bool) {
	v, ok := s.slots.Load(key)
	if !ok {
		return 0, false
	}
	return v.(int), true
}

// slotsOf counts model's governed slots and its ungoverned servers among the
// workers and gangs that take its requests now — the same holders
// modelServable accepts.
func (s *Server) slotsOf(ctx context.Context, model string) (slots, ungoverned int) {
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	if ready, err := s.store.Placements().GetByModel(ctx, model); err == nil {
		for _, p := range ready {
			if p.NodeID == "local" {
				continue
			}
			n, err := s.store.Nodes().Get(ctx, p.NodeID)
			if err != nil || n == nil || !n.TakesNewWork(maxAge, now) || s.engineGoneWhy(p.NodeID) != "" {
				continue
			}
			if k, ok := s.slotsOfKey(p.NodeID); ok {
				slots += k
			} else {
				ungoverned++
			}
		}
	}
	shards, err := s.store.Shards().GetByModel(ctx, model)
	if err != nil || len(shards) == 0 {
		return slots, ungoverned
	}
	alive := s.routableNodes(ctx)
	for key, parts := range store.GroupGangs(shards) {
		coord, ok := gangServable(parts, alive)
		if !ok {
			continue
		}
		if k := s.gangSlots(key, coord); k > 0 {
			slots += k
		} else {
			ungoverned++
		}
	}
	return slots, ungoverned
}

// gangSlots is a gang's slot count: the coordinator's own word, else its head
// worker's heartbeat; 0 = unknown. It is published under the gang's router key
// so the picker reads the same number. The coordinator is asked off the
// request path, once per coordinator address: until it answers, the head's
// number (or none) stands, and its answer signals capacityChanged.
func (s *Server) gangSlots(key store.GangKey, coord store.Shard) int {
	rkey := router.GangKey(key.Model, key.Gang)
	if v, ok := s.gangSlotsAsked.Load(coord.ID); ok && v.(gangSlotsAnswer).addr == coord.Address && v.(gangSlotsAnswer).n > 0 {
		n := v.(gangSlotsAnswer).n
		s.slots.Store(rkey, n)
		return n
	}
	s.askGangSlots(coord)
	if n, ok := s.slotsOfKey(coord.NodeID); ok && coord.NodeID != "" && coord.NodeID != "local" {
		s.slots.Store(rkey, n)
		return n
	}
	s.slots.Delete(rkey)
	return 0
}

// gangSlotsAnswer is what one coordinator said about its slots.
type gangSlotsAnswer struct {
	addr string
	n    int // 0 = asked, no answer (yet)
}

// askGangSlots asks a coordinator for its slot count in the background, once
// per address.
func (s *Server) askGangSlots(coord store.Shard) {
	if coord.Address == "" {
		return
	}
	if v, loaded := s.gangSlotsAsked.LoadOrStore(coord.ID, gangSlotsAnswer{addr: coord.Address}); loaded && v.(gangSlotsAnswer).addr == coord.Address {
		return
	}
	s.gangSlotsAsked.Store(coord.ID, gangSlotsAnswer{addr: coord.Address})
	go func() {
		eng, ok := s.gangEngine(coord)
		if !ok {
			return
		}
		sr, ok := eng.(engines.SlotReporter)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if n, ok := sr.Slots(ctx); ok && n > 0 {
			s.gangSlotsAsked.Store(coord.ID, gangSlotsAnswer{addr: coord.Address, n: n})
			s.capacityChanged()
		}
	}()
}

// ungovernedWorkers names the workers that take new requests and report no
// slot count: while any serves a model, that model's admission is unbounded.
// For /gatewayz, never the request path.
func (s *Server) ungovernedWorkers() []string {
	nodes, err := s.store.Nodes().List(context.Background())
	if err != nil {
		return nil
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	out := []string{}
	for _, n := range nodes {
		if n.ID == "local" || !n.TakesNewWork(maxAge, now) || !s.servesNow(context.Background(), n.ID, "") {
			continue // a gang's parts serve through their head, which is judged by its coordinator
		}
		if _, ok := s.slotsOfKey(n.ID); !ok {
			out = append(out, n.ID)
		}
	}
	sort.Strings(out)
	return out
}
