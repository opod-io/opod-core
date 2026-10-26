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
//   - the KV BOUND (PLAN T23.11, feature "admission_kv_bound"): an engine's
//     slots say how many requests it will RUN at once, not how many its KV
//     pool holds — vLLM's --max-num-seqs is 256 whatever the pool. When the
//     plan states the endpoint's context (its workload block) and the engine
//     states its pool (kv_capacity_tokens, T23.2), a worker or gang counts
//     min(slots, pool ÷ context), so the gate queues at the smaller bound
//     instead of handing the engine requests it can only preempt. It is the
//     pool the engine BUILT, never what is free of it right now: the gate
//     already counts the requests in flight against the bound, and taking
//     their KV off as well would count each of them twice and shrink the
//     bound as requests are admitted.
//
// Every number here is in memory: a sync.Map written on the heartbeat and
// read by the router's picker and the gate's capacity computation. The raw
// engine slots stay what /admin/v1/nodes and a door's mirror read; the
// router's picker and the gate read the bounded number (admitSlotsOfKey). A
// door mirrors only the raw slots, so a door still bounds by slots.

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

// kvRequests is how many requests of context tokens a KV pool of capacity
// tokens holds — at least one, since an engine that started holds the one it
// was sized for. ok = false when either is unknown: no bound.
func kvRequests(capacity *int64, context int) (int, bool) {
	if capacity == nil || *capacity <= 0 || context <= 0 {
		return 0, false
	}
	return max(int(*capacity/int64(context)), 1), true
}

// workerKVPool is the KV pool a worker's engine last said it built, from a
// fresh heartbeat sample; nil when it did not say.
func (s *Server) workerKVPool(nodeID string) *int64 {
	v, ok := s.nodeLoad.Load(nodeID)
	if !ok {
		return nil
	}
	ld := v.(nodeLoadSample)
	if time.Since(ld.at) > loadSampleMaxAge {
		return nil
	}
	return ld.KVCapacityTokens
}

// admitSlotsOfKey is the number of requests the gate and the router's picker
// let a worker (its node id) or a gang (router.GangKey) hold at once: its
// slots, lowered to what its KV pool holds at the endpoint's context (T23.11).
// kv = true when the KV pool is the smaller. A gang's number is bounded where
// it is stored (gangSlots), so only a worker's is bounded here.
func (s *Server) admitSlotsOfKey(key string) (n int, kv, ok bool) {
	n, ok = s.slotsOfKey(key)
	if !ok {
		return 0, false, false
	}
	if k, known := kvRequests(s.workerKVPool(key), s.plan.endpointContext()); known && k < n {
		return k, true, true
	}
	return n, false, true
}

// routerSlots is the router's SlotSource: the bounded number.
func (s *Server) routerSlots(key string) (int, bool) {
	n, _, ok := s.admitSlotsOfKey(key)
	return n, ok
}

// slotsOf counts model's governed slots and its ungoverned servers among the
// workers and gangs that take its requests now — the same holders
// modelServable accepts — and names them by router key (a worker's node id, a
// gang's router.GangKey): the keys a request in flight may be on and still
// hold one of these slots. Each counts its bounded number (admitSlotsOfKey);
// kvBound = true when a KV pool lowered any of them.
func (s *Server) slotsOf(ctx context.Context, model string) (slots, ungoverned int, holders map[string]bool, kvBound bool) {
	holders = map[string]bool{}
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
			holders[p.NodeID] = true
			if k, kv, ok := s.admitSlotsOfKey(p.NodeID); ok {
				slots += k
				kvBound = kvBound || kv
			} else {
				ungoverned++
			}
		}
	}
	shards, err := s.store.Shards().GetByModel(ctx, model)
	if err != nil || len(shards) == 0 {
		return slots, ungoverned, holders, kvBound
	}
	alive := s.routableNodes(ctx)
	for key, parts := range store.GroupGangs(shards) {
		coord, ok := gangServable(parts, alive)
		if !ok {
			continue
		}
		holders[router.GangKey(key.Model, key.Gang)] = true
		if k, kv := s.gangSlots(key, coord); k > 0 {
			slots += k
			kvBound = kvBound || kv
		} else {
			ungoverned++
		}
	}
	return slots, ungoverned, holders, kvBound
}

// gangSlots is a gang's slot count: the coordinator's own word, else its head
// worker's heartbeat; 0 = unknown. It is published under the gang's router key
// so the picker reads the same number. The coordinator is asked off the
// request path, once per coordinator address: until it answers, the head's
// number (or none) stands, and its answer signals capacityChanged.
//
// The number is lowered to what the coordinator's KV pool holds at the
// endpoint's context (T23.11), from the last sample /loadz took of it
// (gangSample) — never a scrape here, which is on the request path. kv = true
// when the pool is the smaller.
func (s *Server) gangSlots(key store.GangKey, coord store.Shard) (n int, kv bool) {
	rkey := router.GangKey(key.Model, key.Gang)
	if v, ok := s.gangSlotsAsked.Load(coord.ID); ok && v.(gangSlotsAnswer).addr == coord.Address && v.(gangSlotsAnswer).n > 0 {
		n = v.(gangSlotsAnswer).n
	} else {
		s.askGangSlots(coord)
		if h, ok := s.slotsOfKey(coord.NodeID); ok && coord.NodeID != "" && coord.NodeID != "local" {
			n = h
		}
	}
	if n <= 0 {
		s.slots.Delete(rkey)
		return 0, false
	}
	if k, known := kvRequests(s.gangKVPool(coord), s.plan.endpointContext()); known && k < n {
		n, kv = k, true
	}
	s.slots.Store(rkey, n)
	return n, kv
}

// gangKVPool is the KV pool a gang's coordinator last said it built, from the
// cached sample /loadz took (gangSample); nil when none is fresh.
func (s *Server) gangKVPool(coord store.Shard) *int64 {
	v, ok := s.gangLoad.Load(coord.ID)
	if !ok {
		return nil
	}
	ld := v.(nodeLoadSample)
	if time.Since(ld.at) > loadSampleMaxAge {
		return nil
	}
	return ld.KVCapacityTokens
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
