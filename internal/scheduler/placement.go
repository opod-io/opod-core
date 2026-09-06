package scheduler

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"strconv"

	"github.com/opod-io/opod/internal/store"
)

func (o *Orchestrator) pickWorkersByID(ctx context.Context, ids []string) ([]store.Node, error) {
	all, err := o.Store.Nodes().List(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.Node, len(all))
	for _, nd := range all {
		byID[nd.ID] = nd
		if nd.Hostname != "" {
			byID[nd.Hostname] = nd
		}
	}
	out := make([]store.Node, 0, len(ids))
	for _, id := range ids {
		nd, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("node %q not found", id)
		}
		if nd.State != "ready" || nd.Address == "" {
			return nil, fmt.Errorf("node %q is not ready", id)
		}
		out = append(out, nd)
	}
	return out, nil
}

func (o *Orchestrator) pickWorkers(ctx context.Context, n int) ([]store.Node, error) {
	all, err := o.Store.Nodes().List(ctx)
	if err != nil {
		return nil, err
	}
	ready := make([]store.Node, 0, len(all))
	for _, nd := range all {
		if nd.State == "ready" && nd.Address != "" {
			ready = append(ready, nd)
		}
	}
	if len(ready) < n {
		return nil, fmt.Errorf("need %d ready workers, have %d", n, len(ready))
	}
	sort.Slice(ready, func(i, j int) bool { return ready[i].RAMGB > ready[j].RAMGB })
	return ready[:n], nil
}

// rollback stops every successfully-launched process so failed creates
// don't leave orphan rpc-servers running on workers.
func (o *Orchestrator) rollback(ctx context.Context, created []store.Shard) {
	for _, s := range created {
		switch s.Role {
		case "coordinator":
			if s.NodeID == "" || s.NodeID == "local" {
				_ = o.Supervisor.Stop(s.ProcessID)
			} else if node, err := o.Store.Nodes().Get(ctx, s.NodeID); err == nil && node != nil {
				_ = o.callWorkerStop(ctx, *node, s.ProcessID)
			}
		case "rpc":
			node, err := o.Store.Nodes().Get(ctx, s.NodeID)
			if err == nil && node != nil {
				_ = o.callWorkerStop(ctx, *node, s.ProcessID)
			}
		}
		_ = o.Store.Shards().Delete(ctx, s.ID)
	}
}

// pickCoordPort returns the first port >= want that no existing shard row on
// the same host already occupies (other coordinators and rpc-servers alike).
// Every catalog entry that omits coordinator_port shares the same default, so
// without this a second model placed on the same host would fail to bind and
// crashloop while its shard row reads "ready".
func (o *Orchestrator) pickCoordPort(ctx context.Context, nodeID string, want int) int {
	existing, err := o.Store.Shards().List(ctx)
	if err != nil {
		o.Log.Warn("could not list shards for coordinator port allocation — using default", "err", err)
		return want
	}
	used := make(map[int]bool)
	for _, s := range existing {
		if s.NodeID != nodeID {
			continue
		}
		if _, p, sErr := net.SplitHostPort(s.Address); sErr == nil {
			if n, aErr := strconv.Atoi(p); aErr == nil {
				used[n] = true
			}
		}
	}
	port := want
	for used[port] {
		port++
	}
	return port
}

// coordinatorChoice describes who'll run the llama-server coordinator.
type coordinatorChoice struct {
	nodeID string      // "local" for leader, else node row id
	local  bool        // true when coordinator runs on the leader's supervisor
	node   *store.Node // populated when local==false; the worker to dial
}

// pickCoordinatorHost picks the strongest host (by RAM) among workers + the
// leader to run the llama-server coordinator. Operators can override via
// env OPOD_COORDINATOR_NODE=<node_id> ("local" forces leader).
//
// Why this exists: the coordinator does the actual layer aggregation across
// rpc-servers. Pinning it to the leader was the v0.4 default and wasted
// capacity when a worker had more RAM than the leader.
func (o *Orchestrator) pickCoordinatorHost(ctx context.Context, workers []store.Node) coordinatorChoice {
	if override := os.Getenv("OPOD_COORDINATOR_NODE"); override != "" {
		if override == "local" {
			return coordinatorChoice{nodeID: "local", local: true}
		}
		for i := range workers {
			if workers[i].ID == override {
				w := workers[i]
				return coordinatorChoice{nodeID: w.ID, node: &w}
			}
		}
		o.Log.Warn("OPOD_COORDINATOR_NODE not in shard worker set — falling back to default", "want", override)
	}

	// Default policy: pick the highest-RAM worker; only fall back to the
	// leader when there are no workers (single-machine sharding test).
	// Operators who want the leader can set OPOD_COORDINATOR_NODE=local.
	if len(workers) == 0 {
		return coordinatorChoice{nodeID: "local", local: true}
	}
	best := workers[0]
	for _, w := range workers[1:] {
		if w.RAMGB > best.RAMGB {
			best = w
		}
	}
	return coordinatorChoice{nodeID: best.ID, node: &best}
}

// ---- HTTP calls to worker process endpoints ----
