package scheduler

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// ErrUnplaceable marks a create refused because its workers could not be
// found. It is returned before the gang being replaced is torn down and
// before any process starts, so a caller that sees it has nothing to clean
// up and must not tear anything down on its own.
var ErrUnplaceable = errors.New("refused, nothing was changed")

// DefaultHeartbeatMaxAge bounds a worker's heartbeat age when the router's
// own check is off (router.heartbeat_max_age_seconds = 0) — the bound the
// leader's liveness rule falls back to.
const DefaultHeartbeatMaxAge = store.DefaultHeartbeatMaxAge

// WorkerFor is THE rule for "may new work be placed on this node row" — a
// shard part, or a model pinned with `model add --node`. Every placer asks
// it: the CLI's shard-count picker (WorkerMemoryFacts), the leader's own pick
// for a count without nodes (pickWorkers) and the check of a caller-named
// list (pickWorkersByID). why is empty when the row qualifies, else a short
// reason fit for an error message.
//
//   - The leader's own "local" row is never a worker: it reads ready and has
//     an address, but that address is the gateway, which has no
//     /v1/process/start — a part placed there fails at launch.
//   - A row must be ready (so a draining node takes nothing new), reachable
//     (an address), and heartbeating within maxAge (≤ 0 = DefaultHeartbeatMaxAge).
func WorkerFor(n store.Node, maxAge time.Duration, now time.Time) (ok bool, why string) {
	if maxAge <= 0 {
		maxAge = DefaultHeartbeatMaxAge
	}
	if n.ID == "local" {
		return false, "the leader's own row, not a worker"
	}
	// The leader-wide rule (store.Node.WhyNoNewWork: drained, not a serving
	// state, heartbeats stopped) — the one the router routes by.
	if why := n.WhyNoNewWork(maxAge, now); why != "" {
		return false, why
	}
	if n.State != store.NodeStateReady {
		return false, "state " + n.State
	}
	if n.Address == "" {
		return false, "no address"
	}
	return true, ""
}

// pickWorkersByID resolves the caller's named nodes (id or hostname), in the
// caller's order. A name that is unknown, or a row WorkerFor refuses, fails
// here with the reason rather than at process start on the wrong machine.
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
	now := time.Now()
	out := make([]store.Node, 0, len(ids))
	for _, id := range ids {
		nd, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("node %q not found", id)
		}
		if ok, why := WorkerFor(nd, o.HeartbeatMaxAge, now); !ok {
			return nil, fmt.Errorf("node %q is not ready (%s)", id, why)
		}
		out = append(out, nd)
	}
	return out, nil
}

// pickWorkers selects the n highest-RAM rows WorkerFor accepts. With fewer
// than n it refuses with the numbers — how many were asked for, which rows
// qualify, and why each other row does not — before anything is launched.
func (o *Orchestrator) pickWorkers(ctx context.Context, n int) ([]store.Node, error) {
	all, err := o.Store.Nodes().List(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ready := make([]store.Node, 0, len(all))
	var have, refused []string
	for _, nd := range all {
		if ok, why := WorkerFor(nd, o.HeartbeatMaxAge, now); !ok {
			refused = append(refused, nd.ID+" — "+why)
			continue
		}
		ready = append(ready, nd)
		have = append(have, nd.ID)
	}
	if len(ready) < n {
		msg := fmt.Sprintf("need %d ready workers, have %d", n, len(ready))
		if len(have) > 0 {
			msg += " (" + strings.Join(have, ", ") + ")"
		}
		if len(refused) > 0 {
			msg += "; not counted: " + strings.Join(refused, "; ")
		}
		return nil, errors.New(msg + " — join more workers (`opod join`), or name a smaller count")
	}
	sort.SliceStable(ready, func(i, j int) bool { return ready[i].RAMGB > ready[j].RAMGB })
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

// portAllocator hands out free TCP ports per node for the length of ONE create.
//
// Two things make a single "is this port taken?" lookup insufficient once a
// model can have several gangs. First, a port is free or taken per NODE, and
// two gangs of one model may share a node. Second — the part a per-call lookup
// cannot get right — the ports chosen earlier in the same create are not in the
// store yet, so each part would be told the same port was free. The allocator
// reads the store once and then remembers what it has handed out.
type portAllocator struct{ used map[string]map[int]bool }

// newPortAllocator reads every shard row once and records the port each one
// occupies, per node. A store it cannot read yields an empty allocator: the
// caller then gets the port it asked for, which is the old behaviour and no
// worse than refusing to place at all.
func (o *Orchestrator) newPortAllocator(ctx context.Context) *portAllocator {
	a := &portAllocator{used: map[string]map[int]bool{}}
	existing, err := o.Store.Shards().List(ctx)
	if err != nil {
		o.Log.Warn("could not list shards for port allocation — ports may collide", "err", err)
		return a
	}
	for _, s := range existing {
		if _, p, sErr := net.SplitHostPort(s.Address); sErr == nil {
			if n, aErr := strconv.Atoi(p); aErr == nil {
				a.mark(s.NodeID, n)
			}
		}
	}
	return a
}

func (a *portAllocator) mark(nodeID string, port int) {
	if a.used[nodeID] == nil {
		a.used[nodeID] = map[int]bool{}
	}
	a.used[nodeID][port] = true
}

// take returns the first free port at or above want on this node, and records
// it so the next caller in the same create cannot be given the same one.
func (a *portAllocator) take(nodeID string, want int) int {
	port := want
	for a.used[nodeID][port] {
		port++
	}
	a.mark(nodeID, port)
	return port
}

// takeRange reserves a contiguous block of span+1 ports on this node and
// returns its first port. Ray wants a RANGE for the ephemeral ports its
// workers open, and two Ray daemons on one host must not be handed overlapping
// ones — the second to start fails to bind and its gang never forms.
func (a *portAllocator) takeRange(nodeID string, want, span int) int {
	base := want
	for {
		free := true
		for p := base; p <= base+span; p++ {
			if a.used[nodeID][p] {
				free = false
				base = p + 1 // skip past the taken port, never retry inside the block
				break
			}
		}
		if free {
			for p := base; p <= base+span; p++ {
				a.mark(nodeID, p)
			}
			return base
		}
	}
}

// coordinatorChoice describes who'll run the llama-server coordinator.
type coordinatorChoice struct {
	nodeID string      // "local" for leader, else node row id
	local  bool        // true when coordinator runs on the leader's supervisor
	node   *store.Node // populated when local==false; the worker to dial
}

// pickCoordinatorHost picks the strongest host (by RAM) among workers + the
// leader to run the llama-server coordinator. Operators can override with
// OPOD_COORDINATOR_NODE=<node_id> ("local" forces the leader), carried in
// o.CoordinatorNode through the config contract.
//
// Why this exists: the coordinator does the actual layer aggregation across
// rpc-servers. Pinning it to the leader was the v0.4 default and wasted
// capacity when a worker had more RAM than the leader.
func (o *Orchestrator) pickCoordinatorHost(ctx context.Context, workers []store.Node) coordinatorChoice {
	if override := o.CoordinatorNode; override != "" {
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
