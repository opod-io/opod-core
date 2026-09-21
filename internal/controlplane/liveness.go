package controlplane

import (
	"context"
	"sort"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// Liveness — one rule for "is this worker still there?", applied everywhere
// the leader speaks about its workers: /readyz, the waking 503, the node and
// shard listings, and the router's shard-coordinator lookup.
//
// Workers heartbeat every 5 s. Node and shard rows are written on register /
// create and never expire on their own, so without this rule a worker whose
// pod vanished (node NotReady, preempted, killed) stays "ready" forever, the
// leader keeps answering /readyz 200, Kubernetes keeps routing to it, and
// every request 503s. Verified on a design-partner cell 2026-09-03: both parts of a
// PP2 gang were gone for 1h45m and the leader still reported
// "shard-coordinator ready".
//
// The rule is derived, not stored: rows keep their last written state and a
// returning heartbeat makes them live again with no reconciliation step.

// NodeStateLost is the derived state reported for a node whose heartbeat is
// older than router.heartbeat_max_age_seconds. It is never persisted. The
// rule itself lives with the row (store.Node.Alive / TakesNewWork /
// LiveState) so the router, the shard pickers and the CLI ask the same one.
const NodeStateLost = store.NodeStateLost

// ShardStatusLost is the derived status reported for a shard part whose node
// is lost. It is never persisted.
const ShardStatusLost = "lost"

// heartbeatMaxAge is the staleness bound for worker heartbeats. Config 0
// (check disabled for routing) still needs a bound here; 60 s keeps the old
// hasServingCapacity default.
//
// A GATEWAY has NO bound (0 = no age rule, store.Node.Alive). It receives no
// heartbeat: the timestamps on its mirrored rows say when the LEADER last saw
// each worker, and while the leader restarts — every rollout, every failover —
// the mirror cannot refresh them, so they slide past any bound and the door
// refuses workers that are serving. Measured on the design-partner cell
// (2026-09-21): with the leader pod deleted a door served 18 requests and then
// answered 503 to 25, which is the exact failure doors exist to prevent.
//
// What judges a worker on a door is the brain's own verdict, which travels with
// the row (the mirror copies the leader's DERIVED state and a lost, draining or
// engine-silent worker is out). The rest is fail-static by design (ADR-063):
// while the brain is unreachable a door keeps routing to the workers it knows,
// a worker that has really gone fails the dispatch, and how stale the list is
// is published on /gatewayz rather than hidden.
func (s *Server) heartbeatMaxAge() time.Duration {
	if s.isGateway() {
		return 0
	}
	return store.HeartbeatBound(s.cfg.Router.HeartbeatMaxAgeSeconds)
}

// nodeAlive reports whether n heartbeated within the staleness bound. The
// leader's own "local" node never heartbeats and is always alive.
func nodeAlive(n store.Node, maxAge time.Duration, now time.Time) bool {
	return n.Alive(maxAge, now)
}

// liveNodeState is the state the leader reports for n: the stored state,
// except that a node believed ready (or still joining) whose heartbeats
// stopped is reported as lost. Draining/removed states are kept as written.
func liveNodeState(n store.Node, alive bool) string {
	if alive {
		return n.State
	}
	return store.StateWhenSilent(n.State)
}

// aliveNodes returns id → alive for every registered node. Unknown ids
// (a shard row pointing at a node that was removed) look up as false;
// "local" is always true.
func (s *Server) aliveNodes(ctx context.Context) map[string]bool {
	out := map[string]bool{"local": true}
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return out
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for _, n := range nodes {
		out[n.ID] = nodeAlive(n, maxAge, now)
	}
	return out
}

// routableNodes is aliveNodes minus the draining ones: the nodes new work may
// be sent to. A listing reports a draining node as what it is (alive, state
// "draining"); readiness and the waking 503 must not count on it.
func (s *Server) routableNodes(ctx context.Context) map[string]bool {
	out := map[string]bool{"local": true}
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return out
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for _, n := range nodes {
		// The stored rule, plus the one thing only the worker can say: that its
		// engine is gone (T11.2). A gang part whose pod is terminating still
		// heartbeats through the grace period, so without this the gang reads
		// servable for as long as the heartbeat bound and every request in that
		// window is a 502.
		out[n.ID] = n.TakesNewWork(maxAge, now) && s.engineGoneWhy(n.ID) == ""
	}
	return out
}

// shardAlive reports whether a shard part's node is alive. Parts hosted by
// the leader itself (NodeID "local" or empty, the pre-worker-head layout)
// are alive as long as the leader is.
func shardAlive(sh store.Shard, alive map[string]bool) bool {
	if sh.NodeID == "" || sh.NodeID == "local" {
		return true
	}
	return alive[sh.NodeID]
}

// liveShardStatus is the status the leader reports for a shard part: the
// stored status, or lost when its node stopped heartbeating.
func liveShardStatus(sh store.Shard, alive map[string]bool) string {
	if shardAlive(sh, alive) {
		return sh.Status
	}
	return ShardStatusLost
}

// servableShardModels returns the model ids that at least ONE gang can serve
// right now: a ready coordinator AND every part of that gang on an alive node.
// llama-server does not survive a lost rpc backend, so one lost part takes its
// gang out of rotation.
//
// Judged per gang. A model with two gangs is servable while either one is
// whole — asking this over the model's parts as one set meant a single lost
// part hid a healthy gang that was serving requests at that moment, and a
// ready coordinator in one gang vouched for a broken sibling.
func servableShardModels(shards []store.Shard, alive map[string]bool) map[string]bool {
	out := map[string]bool{}
	for key, parts := range store.GroupGangs(shards) {
		if _, ok := gangServable(parts, alive); ok {
			out[key.Model] = true
		}
	}
	return out
}

// gangServable is THE rule for "can this gang serve right now?", asked of one
// gang's parts: its coordinator is ready and not one part is lost. It returns
// the coordinator row because the callers that want more than a yes/no want
// exactly that row — it is the process that holds the KV cache and answers
// requests (loadstats.go), while the parts are rpc-servers.
//
// It is one function because it was three, written out per caller, and that is
// how a gang came to be judged over the model's parts as one set in five
// places (fixed 2026-09-20).
func gangServable(parts []store.Shard, alive map[string]bool) (coord store.Shard, ok bool) {
	found, partLost := false, false
	for _, sh := range parts {
		if !shardAlive(sh, alive) {
			partLost = true
		}
		if sh.Role == "coordinator" && sh.Status == "ready" {
			coord, found = sh, true
		}
	}
	if !found || partLost {
		return store.Shard{}, false
	}
	return coord, true
}

// servableGangCoordinators is the coordinator row of every gang of model that
// can serve right now — the same per-gang judgement as servableShardModels,
// kept as the rows the leader can dial.
func servableGangCoordinators(shards []store.Shard, alive map[string]bool, model string) []store.Shard {
	var out []store.Shard
	for key, parts := range store.GroupGangs(shards) {
		if model != "" && key.Model != model {
			continue
		}
		if coord, ok := gangServable(parts, alive); ok {
			out = append(out, coord)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// servableGangNodes: the nodes holding a part of a gang of model that can serve
// right now — the same per-gang judgement as servableShardModels, kept as the
// node set instead of the answer.
//
// A gang's parts are capacity, and they are the ONLY capacity a sharded
// endpoint has: they hold the cards and they do the work. They hold no
// placement row, though, because the router reaches them through the gang, so
// anything that counts workers by their placements counts none of them (the
// defect /loadz had: a serving two-part gang reported workers 0, and with it
// kv_used_pct, queue_depth and tokens_per_s all 0 — three of the five signals
// an autoscaler is given for that endpoint).
func servableGangNodes(shards []store.Shard, alive map[string]bool, model string) map[string]bool {
	out := map[string]bool{}
	for key, parts := range store.GroupGangs(shards) {
		if model != "" && key.Model != model {
			continue
		}
		if _, ok := gangServable(parts, alive); !ok {
			continue
		}
		for _, sh := range parts {
			if sh.NodeID != "" && sh.NodeID != "local" {
				out[sh.NodeID] = true
			}
		}
	}
	return out
}

// hasServableShardGroup reports whether any sharded model can serve now. A
// gang with a part on a draining node is out with it — the router does not
// send it new requests (router.shardGroupRoutable), so it is not capacity.
func (s *Server) hasServableShardGroup(ctx context.Context) bool {
	shards, err := s.store.Shards().List(ctx)
	if err != nil || len(shards) == 0 {
		return false
	}
	return len(servableShardModels(shards, s.routableNodes(ctx))) > 0
}

// hasAwakeWorkers reports whether ANY registered worker is up and takes new
// work — whatever it is doing with it. It is deliberately the weakest question
// the leader can ask about its fleet: not "can we serve" (hasServingCapacity)
// and not "is there a ready row" (hasLivePlacement), only "is something
// running out there".
//
// It exists to split the two states a floor-0 plan folds together (readyz):
// PARKED, where the workers are gone and zero capacity is the plan, and
// AWAKE-BUT-NOT-SERVING, where the pods are back and registered and the model
// or the gang has not come up. The second is a wake in progress — or a wake
// that failed and will never finish — and reporting it as the first hides it.
//
// One heartbeat bound of slack, by construction: a worker that has just been
// parked keeps heartbeating through its termination grace period, so the
// window right after a park reads `waking`. Both states answer 200, so the
// cost is a label, and the honest reading of that window is that the endpoint
// is neither parked yet nor able to serve.
func (s *Server) hasAwakeWorkers(ctx context.Context) bool {
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return false
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for _, n := range nodes {
		if n.ID == "local" {
			continue
		}
		if n.TakesNewWork(maxAge, now) {
			return true
		}
	}
	return false
}

// hasLivePlacement reports whether any alive non-local worker the router
// would choose has a ready placement — the router-only readiness condition.
// A draining worker is alive but takes no new request, so it does not count:
// when every worker holding the model drains, requests get the waking 503.
func (s *Server) hasLivePlacement(ctx context.Context) bool {
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return false
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for _, n := range nodes {
		if n.ID == "local" || !n.TakesNewWork(maxAge, now) {
			continue
		}
		ps, err := s.store.Placements().GetByNode(ctx, n.ID)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if p.Status == "" || p.Status == "ready" {
				return true
			}
		}
	}
	return false
}

// hasSleepingPlacement: at least one ALIVE worker holds a placement in the
// sleep tier (build item 13) — resident, not routable, wakes on demand.
func (s *Server) hasSleepingPlacement(ctx context.Context) bool {
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return false
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for _, n := range nodes {
		if n.ID == "local" || !nodeAlive(n, maxAge, now) {
			continue
		}
		ps, err := s.store.Placements().GetByNode(ctx, n.ID)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if p.Status == PlacementSleeping {
				return true
			}
		}
	}
	return false
}
