package router

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/store"
)

func (r *Router) pick(ctx context.Context, model string) (engines.Engine, string, error) {
	if model == "" {
		metrics.ObserveRouterPick("local", "ok")
		return r.local, r.localNode, nil
	}

	// 0. Is this a SHARDED model? If yes, route to its coordinator (always
	//    local today) via a llamacpp engine. The coordinator handles the
	//    fan-out to rpc-server backends on workers internally.
	if eng, ok := r.shardCoordinator(ctx, model); ok {
		metrics.ObserveRouterPick("shard", "ok")
		return eng, "shard:" + model, nil
	}

	// 1. Is the model on the local node?
	localHas, _ := r.modelOnNode(ctx, r.localNode, model)
	if localHas {
		metrics.ObserveRouterPick("local", "ok")
		return r.local, r.localNode, nil
	}

	// 2. Find workers hosting it
	placements, err := r.store.Placements().GetByModel(ctx, model)
	if err != nil {
		// Fall back to local — surface its error rather than hiding ours
		metrics.ObserveRouterPick("fallback-to-local", "store-error")
		return r.local, r.localNode, nil
	}
	// Filter out local (if present) — we already checked above
	workers := placements[:0]
	for _, p := range placements {
		if p.NodeID != r.localNode {
			workers = append(workers, p)
		}
	}

	if len(workers) == 0 {
		// Nothing has it — let local try, it will return a clear error.
		metrics.ObserveRouterPick("fallback-to-local", "no-workers")
		return r.local, r.localNode, nil
	}

	// 3. Pick least-loaded worker. The snapshot we sort against is
	//    consistent under RLock, but the actual inflight increment
	//    happens in the caller AFTER we return — so two concurrent
	//    requests can both pick the same "least-loaded" node and
	//    over-route once before the counter catches up. This is a
	//    load-balancing imperfection, not a correctness bug, and
	//    self-corrects on the next request.
	r.mu.RLock()
	sort.Slice(workers, func(i, j int) bool {
		return r.inflight[workers[i].NodeID] < r.inflight[workers[j].NodeID]
	})
	r.mu.RUnlock()

	// 3a. Sticky pin: if there's a fresh (user_id, model) entry whose
	// node is still in the workers list AND not in cooldown, surface
	// it to the front of the sorted slice so it's tried before the
	// least-loaded candidate. KV-cache locality outweighs a small
	// inflight delta on the alternative.
	stickyNode := r.stickyPick(ctx, model)
	if stickyNode != "" {
		workers = preferNode(workers, stickyNode)
	}

	// Walk the sorted list: skip any worker whose heartbeat is stale
	// before falling back to local. Without this, a request to a model
	// that's still in the placements table for a dead node would wait
	// for the engine call to time out.
	for _, pick := range workers {
		node, err := r.store.Nodes().Get(ctx, pick.NodeID)
		if err != nil || node == nil || node.Address == "" {
			metrics.ObserveRouterPick("worker", "error")
			continue
		}
		if r.heartbeatMaxAge > 0 && !node.LastHeartbeat.IsZero() &&
			time.Since(node.LastHeartbeat) > r.heartbeatMaxAge {
			metrics.ObserveRouterPick("worker", "stale-heartbeat")
			if r.log != nil {
				r.log.Warn("router skipping stale worker",
					"node", pick.NodeID,
					"model", model,
					"last_heartbeat", node.LastHeartbeat,
					"max_age", r.heartbeatMaxAge,
				)
			}
			continue
		}
		if r.inCooldown(node.ID) {
			metrics.ObserveRouterPick("worker", "cooldown")
			continue
		}
		eng := r.getOrCreateRemote(node.ID, node.Address, node.WorkerToken)
		// Record the sticky outcome only when a pin was actually consulted —
		// stickyPick already emitted "miss"/"expired" for the no-pin and
		// expired cases, so emitting again here (the old default "miss")
		// double-counted every non-sticky request. When a pin existed we
		// landed on it ("hit") or had to route elsewhere ("miss").
		if stickyNode != "" {
			if node.ID == stickyNode {
				metrics.ObserveStickyOutcome("hit")
			} else {
				metrics.ObserveStickyOutcome("miss")
			}
		}
		// NB: the pin is refreshed on the caller's *success* path
		// (Chat/Embed), not here at pick time — a node that's picked but then
		// fails the engine call shouldn't be pinned for the next turn.
		metrics.ObserveRouterPick("worker", "ok")
		return eng, node.ID, nil
	}
	// All workers exhausted (all dead or stale). Fall back to local — it
	// will surface its own "model not loaded" error.
	metrics.ObserveRouterPick("fallback-to-local", "all-workers-stale")
	return r.local, r.localNode, nil
}

// shardCoordinator returns the llamacpp engine pointing at the coordinator
// of a sharded model, or (nil, false) if the model isn't sharded.
func (r *Router) shardCoordinator(ctx context.Context, modelID string) (engines.Engine, bool) {
	cacheKey := "shard:" + modelID
	r.mu.RLock()
	if eng, ok := r.remotes[cacheKey]; ok {
		r.mu.RUnlock()
		return eng, true
	}
	r.mu.RUnlock()
	shards, err := r.store.Shards().GetByModel(ctx, modelID)
	if err != nil || len(shards) == 0 {
		return nil, false
	}
	// One lost part takes the gang out: llama-server does not survive a
	// dead rpc backend, so a coordinator on a live node with a stale
	// sibling is not a route either (same rule as the leader's /readyz).
	if !r.shardGroupAlive(ctx, shards) {
		return nil, false
	}
	for _, s := range shards {
		if s.Role == "coordinator" && s.Status == "ready" {
			eng := engines.MustNew("llamacpp", "http://"+s.Address, "")
			r.mu.Lock()
			r.remotes[cacheKey] = eng
			r.mu.Unlock()
			return eng, true
		}
	}
	return nil, false
}

// shardGroupAlive reports whether every part of a gang sits on a worker
// that heartbeated within heartbeatMaxAge. Parts hosted by the leader
// itself ("local" / empty node id) are always alive; 0 max age disables
// the check (legacy behaviour).
func (r *Router) shardGroupAlive(ctx context.Context, shards []store.Shard) bool {
	if r.heartbeatMaxAge <= 0 {
		return true
	}
	now := time.Now()
	for _, sh := range shards {
		if sh.NodeID == "" || sh.NodeID == "local" {
			continue
		}
		n, err := r.store.Nodes().Get(ctx, sh.NodeID)
		if err != nil || n == nil || now.Sub(n.LastHeartbeat) > r.heartbeatMaxAge {
			return false
		}
	}
	return true
}

// InvalidateModel drops any cached engine for the given model. Called by
// the orchestrator when shards are torn down so the next request rebuilds.
func (r *Router) InvalidateModel(modelID string) {
	r.mu.Lock()
	delete(r.remotes, "shard:"+modelID)
	r.mu.Unlock()
}

// InvalidateNode drops all per-node router state for a deleted node: the
// cached remote engine plus cooldown, failure, inflight, and sticky-pin
// entries keyed by it. Called by the control plane when a node is removed
// so a re-registered node (possibly at a new address / token) gets a
// fresh engine instead of the stale cached one.
func (r *Router) InvalidateNode(nodeID string) {
	if nodeID == "" {
		return
	}
	r.mu.Lock()
	delete(r.remotes, nodeID)
	delete(r.cooldowns, nodeID)
	delete(r.failures, nodeID)
	delete(r.inflight, nodeID)
	prefix := nodeID + "|"
	for k := range r.inflightDim {
		if strings.HasPrefix(k, prefix) {
			delete(r.inflightDim, k)
		}
	}
	for k, e := range r.stickiness {
		if e.NodeID == nodeID {
			delete(r.stickiness, k)
		}
	}
	cooldowns := len(r.cooldowns)
	r.mu.Unlock()
	metrics.SetRouterCooldownsActive(cooldowns)
	metrics.SetRouterInflight(nodeID, 0)
}

func (r *Router) modelOnNode(ctx context.Context, nodeID, modelID string) (bool, error) {
	ps, err := r.store.Placements().GetByNode(ctx, nodeID)
	if err != nil {
		return false, err
	}
	for _, p := range ps {
		if p.ModelID == modelID && p.Status == "ready" {
			return true, nil
		}
	}
	return false, nil
}

// getOrCreateRemote returns a cached remote engine (vLLM driver pointing at the
// worker's address) or builds + caches one.
//
// Concurrency: holds the write lock for the entire check-and-create so
// two concurrent calls for the same nodeID can't each construct a fresh
// engine and have one silently overwrite the other. The window is short
// (constructing a driver is a small struct alloc, no I/O) so write-lock for
// the duration is fine.
func (r *Router) getOrCreateRemote(nodeID, address, token string) engines.Engine {
	r.mu.Lock()
	defer r.mu.Unlock()
	if eng, ok := r.remotes[nodeID]; ok {
		return eng
	}
	endpoint := address
	if !startsWithScheme(endpoint) {
		endpoint = "http://" + endpoint
	}
	// Remote opod workers speak the OpenAI wire; the vllm driver is that client.
	eng := engines.MustNew("vllm", endpoint, token)
	r.remotes[nodeID] = eng
	return eng
}

func (r *Router) incInflight(nodeID, model string) {
	r.mu.Lock()
	r.inflight[nodeID]++
	r.inflightDim[nodeID+"|"+model]++
	n := r.inflight[nodeID]
	r.mu.Unlock()
	metrics.SetRouterInflight(nodeID, n)
}

func (r *Router) decInflight(nodeID, model string) {
	r.mu.Lock()
	if r.inflight[nodeID] > 0 {
		r.inflight[nodeID]--
	}
	key := nodeID + "|" + model
	if r.inflightDim[key] > 0 {
		r.inflightDim[key]--
	}
	if r.inflightDim[key] == 0 {
		delete(r.inflightDim, key) // keep the map from growing unboundedly
	}
	n := r.inflight[nodeID]
	r.mu.Unlock()
	metrics.SetRouterInflight(nodeID, n)
}

// Inflight returns a snapshot of current per-node in-flight counts (used by
// the admin /admin/v1/router endpoint).
func (r *Router) Inflight() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.inflight))
	for k, v := range r.inflight {
		out[k] = v
	}
	return out
}

// InflightByModel returns a snapshot of per-(node, model) in-flight counts,
// keyed `node_id + "|" + model` (the model string is whatever the request
// carried — catalog id normally, engine-native for direct passthrough).
// Used by the lifecycle manager's eviction drain so it waits only on the
// victim's traffic, not the whole node's.
func (r *Router) InflightByModel() map[string]int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]int, len(r.inflightDim))
	for k, v := range r.inflightDim {
		out[k] = v
	}
	return out
}

// RegisterLocalModel records that the local engine has loaded a model. Called
// from cmd_model after a successful local pull, and at startup for any models
// the local engine already has.
func (r *Router) RegisterLocalModel(ctx context.Context, modelID string) error {
	return r.store.Placements().Upsert(ctx, store.Placement{
		NodeID:   r.localNode,
		ModelID:  modelID,
		Status:   "ready",
		LastSeen: time.Now(),
	})
}

func startsWithScheme(s string) bool {
	for i := 0; i+3 < len(s); i++ {
		if s[i] == ':' && s[i+1] == '/' && s[i+2] == '/' {
			return true
		}
	}
	return false
}

// ensure interface satisfaction at compile time
var _ engines.Engine = (*Router)(nil)
