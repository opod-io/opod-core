package controlplane

import (
	"context"
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
// older than router.heartbeat_max_age_seconds. It is never persisted.
const NodeStateLost = "lost"

// ShardStatusLost is the derived status reported for a shard part whose node
// is lost. It is never persisted.
const ShardStatusLost = "lost"

// heartbeatMaxAge is the staleness bound for worker heartbeats. Config 0
// (check disabled for routing) still needs a bound here; 60 s keeps the old
// hasServingCapacity default.
func (s *Server) heartbeatMaxAge() time.Duration {
	if s.cfg.Router.HeartbeatMaxAgeSeconds > 0 {
		return time.Duration(s.cfg.Router.HeartbeatMaxAgeSeconds) * time.Second
	}
	return 60 * time.Second
}

// nodeAlive reports whether n heartbeated within the staleness bound. The
// leader's own "local" node never heartbeats and is always alive.
func nodeAlive(n store.Node, maxAge time.Duration, now time.Time) bool {
	if n.ID == "local" {
		return true
	}
	return now.Sub(n.LastHeartbeat) <= maxAge
}

// liveNodeState is the state the leader reports for n: the stored state,
// except that a node believed ready (or still joining) whose heartbeats
// stopped is reported as lost. Draining/removed states are kept as written.
func liveNodeState(n store.Node, alive bool) string {
	if alive {
		return n.State
	}
	switch n.State {
	case "ready", "joining", "":
		return NodeStateLost
	}
	return n.State
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

// servableShardModels returns the model ids whose gang can serve right now:
// a ready coordinator AND every part of that model (coordinator and rpc
// backends) on an alive node. llama-server does not survive a lost rpc
// backend, so one lost part takes the whole gang out of rotation.
func servableShardModels(shards []store.Shard, alive map[string]bool) map[string]bool {
	coordinatorReady := map[string]bool{}
	partLost := map[string]bool{}
	for _, sh := range shards {
		if !shardAlive(sh, alive) {
			partLost[sh.ModelID] = true
		}
		if sh.Role == "coordinator" && sh.Status == "ready" {
			coordinatorReady[sh.ModelID] = true
		}
	}
	out := map[string]bool{}
	for model := range coordinatorReady {
		if !partLost[model] {
			out[model] = true
		}
	}
	return out
}

// hasServableShardGroup reports whether any sharded model can serve now.
func (s *Server) hasServableShardGroup(ctx context.Context) bool {
	shards, err := s.store.Shards().List(ctx)
	if err != nil || len(shards) == 0 {
		return false
	}
	return len(servableShardModels(shards, s.aliveNodes(ctx))) > 0
}

// hasLivePlacement reports whether any alive non-local worker has a ready
// placement — the router-only readiness condition.
func (s *Server) hasLivePlacement(ctx context.Context) bool {
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
