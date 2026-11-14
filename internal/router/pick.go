package router

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/metrics"
	"github.com/opod-io/opod-core/internal/store"
)

func (r *Router) pick(ctx context.Context, model string) (engines.Engine, string, error) {
	if model == "" {
		metrics.ObserveRouterPick("local", "ok")
		return r.local, r.localNode, nil
	}

	// 0. Is this a SHARDED model? If yes, route to its coordinator (always
	//    local today) via a llamacpp engine. The coordinator handles the
	//    fan-out to rpc-server backends on workers internally.
	if eng, key, ok := r.shardCoordinator(ctx, model); ok {
		metrics.ObserveRouterPick("shard", "ok")
		return eng, key, nil
	} else if skippedEveryGang(ctx, model) {
		// Every routable gang was set aside by THIS request (nextworker.go):
		// the answer is "unreachable", never the leader's own engine, which
		// does not hold a sharded model.
		metrics.ObserveRouterPick("shard", "none-reachable")
		return nil, "", noWorkerLeft(model)
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
	// Workers that take no new request leave the candidate list HERE, before
	// roles, revision groups and load scores see them — one filter, one rule
	// (takesRequests: the store's TakesNewWork, an address, not in cooldown).
	// So "live" means the same thing to every step below: a revision whose
	// only worker is draining, lost or cooling down is a revision with no
	// worker and its share goes to the rest, and the load-aware scorer never
	// ranks a node the walk would have skipped. What is in flight on such a
	// node is not touched — it finishes on the engine it started on.
	workers, nodes := r.takingRequests(ctx, model, workers)
	if len(workers) == 0 {
		// Every holder is draining or lost (one only in the penalty box is
		// still asked: takingRequests). Fall back to local —
		// it will surface its own "model not loaded" error.
		metrics.ObserveRouterPick("fallback-to-local", "all-workers-stale")
		return r.local, r.localNode, nil
	}
	// Workers this very request already found unreachable (nextworker.go).
	if workers = withoutSkipped(ctx, workers); len(workers) == 0 {
		metrics.ObserveRouterPick("worker", "none-reachable")
		return nil, "", noWorkerLeft(model)
	}
	// The capabilities each candidate registered, read once per candidate per
	// pick and parsed only when the blob is new to this router (nodecaps.go).
	caps := make(map[string]nodeCaps, len(nodes))
	for id, n := range nodes {
		caps[id] = r.capsOf(n)
	}
	// Roles (roles.go): generation never lands on a prefill half while a
	// decode half is alive.
	workers = decodeOnly(workers, func(id string) string { return caps[id].role })

	// Revision weights (R15.17): when the policy splits traffic between plan
	// revisions and more than one is serving, the group is chosen first and the
	// load-aware ordering below then runs INSIDE it. A revision with no live
	// worker is skipped, so a restarting canary pod never black-holes its share.
	workers = r.pickRevisionGroup(ctx, model, workers, func(id string) int { return caps[id].revision })

	// 3. Pick least-loaded worker. The snapshot we sort against is
	//    consistent under RLock, but the actual inflight increment
	//    happens in the caller AFTER we return — so two concurrent
	//    requests can both pick the same "least-loaded" node and
	//    over-route once before the counter catches up. This is a
	//    load-balancing imperfection, not a correctness bug, and
	//    self-corrects on the next request.
	//    Load-aware (load.go): the engine's own KV use and queue join the
	//    in-flight count, and a saturated worker goes to the back of the
	//    line — it stops receiving new requests before it errors.
	cands := make([]candidate, len(workers))
	for i, w := range workers {
		cands[i] = workerCandidate(w.NodeID, caps[w.NodeID].kvPeer)
	}
	// The shared ranking (rank.go): load with the block credit, saturated
	// last, then the prefix pin, then the conversation's pin — the same order
	// a gang is ranked by. Equal workers keep their placement order.
	order, sticky := r.rankCandidates(ctx, model, cands, nil)

	// The list holds only workers that take requests (takingRequests above)
	// and is never empty here (every step after it keeps at least one), in
	// order: the first is the pick.
	node := nodes[order[0].key]
	eng := r.getOrCreateRemote(node.ID, node.Address, node.WorkerToken, r.capsOf(node).engine)
	notePinOutcome(ctx, sticky, node.ID)
	// NB: the pin is refreshed on the caller's *success* path
	// (Chat/Embed), not here at pick time — a node that's picked but then
	// fails the engine call shouldn't be pinned for the next turn.
	metrics.ObserveRouterPick("worker", "ok")
	return eng, node.ID, nil
}

// takesRequests is the router's whole answer to "may a new request go to
// this worker?": the leader-wide rule (store.Node.WhyNoNewWork — not drained,
// a serving state, heartbeating within heartbeatMaxAge; 0 = no age rule),
// plus the two things only a router knows: it needs an address to dial, and
// it keeps a penalty box. why is the metric label of a refusal.
func (r *Router) takesRequests(n *store.Node, now time.Time) (ok bool, why string) {
	switch {
	case n == nil || n.Address == "":
		return false, "error"
	case n.Draining():
		return false, "draining"
	case !n.TakesNewWork(r.heartbeatMaxAge, now):
		return false, "stale-heartbeat"
	case r.engineDownWhy(n.ID) != "":
		// The worker said its engine is gone — a pod being terminated keeps
		// heartbeating through its grace period, and routing into it is a 502
		// the caller cannot act on (T11.2). Only a statement the worker MADE
		// counts here: silence is the heartbeat rule above.
		return false, "engine-stopped"
	case r.inCooldown(n.ID):
		return false, "cooldown"
	}
	return true, ""
}

// staleEpisode records whether nodeID is stale now and reports true only when
// it BECAME stale: the WARN is for that moment, not for every pick that walks
// past the row. A worker whose pod was replaced keeps its row indefinitely, and a
// per-pick warning was one log line per request per dead row. The metric still
// counts every skip.
func (r *Router) staleEpisode(nodeID string, stale bool) (began bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !stale {
		delete(r.staleWarned, nodeID)
		return false
	}
	if r.staleWarned[nodeID] {
		return false
	}
	r.staleWarned[nodeID] = true
	return true
}

// takingRequests narrows placements to workers takesRequests accepts and
// returns their node rows, read once for every later step of the pick.
func (r *Router) takingRequests(ctx context.Context, model string, ps []store.Placement) ([]store.Placement, map[string]*store.Node) {
	now := time.Now()
	out := ps[:0:0]
	var cooling []store.Placement // refused only for the penalty box
	nodes := make(map[string]*store.Node, len(ps))
	for _, p := range ps {
		n, err := r.store.Nodes().Get(ctx, p.NodeID)
		if err != nil {
			n = nil
		}
		ok, why := r.takesRequests(n, now)
		if r.staleEpisode(p.NodeID, why == "stale-heartbeat") && r.log != nil {
			r.log.Warn("router skipping stale worker", "node", p.NodeID, "model", model,
				"last_heartbeat", n.LastHeartbeat, "max_age", r.heartbeatMaxAge)
		}
		if !ok {
			metrics.ObserveRouterPick("worker", why)
			if why == "cooldown" {
				cooling = append(cooling, p)
				nodes[p.NodeID] = n
			}
			continue
		}
		nodes[p.NodeID] = n
		out = append(out, p)
	}
	// The penalty box is a preference, not a verdict: when every worker that
	// is otherwise live is in it, asking one is better than answering from the
	// leader's own engine, which does not hold the model. A worker whose engine
	// restarted in two seconds is served again at once; one still down answers
	// "nothing" and the request ends as no capacity (503 + Retry-After).
	if len(out) == 0 && len(cooling) > 0 {
		return cooling, nodes
	}
	for _, p := range cooling {
		delete(nodes, p.NodeID)
	}
	return out, nodes
}

// shardCoordinator picks a gang of a sharded model and returns the engine
// pointing at that gang's coordinator, plus the pseudo node id the caller
// accounts the request under. (nil, "", false) if the model is not sharded or
// no gang can serve.
//
// A model may have several gangs (build item 6). They are independent copies of
// the same weights, each serving whole requests, so this is a PICK among them —
// the same shape as picking among workers, and for the same reason: the least
// loaded one should get the request.
//
// Every judgement here is made per gang and never over the union of a model's
// parts. Over the union, one gang losing a part looked like the model losing a
// part and took healthy siblings out with it, and one gang's ready coordinator
// made a broken sibling look routable.
func (r *Router) shardCoordinator(ctx context.Context, modelID string) (engines.Engine, string, bool) {
	shards, err := r.store.Shards().GetByModel(ctx, modelID)
	if err != nil || len(shards) == 0 {
		return nil, "", false
	}
	gangs := store.GroupGangs(shards)
	ids := make([]string, 0, len(gangs))
	for k := range gangs {
		ids = append(ids, k.Gang)
	}
	sort.Strings(ids) // a stable order, so an unloaded fleet does not pick at random

	var ready []gangCandidate
	for _, id := range ids {
		parts := gangs[store.GangKey{Model: modelID, Gang: id}]
		// One lost part takes ITS gang out: llama-server does not survive a
		// dead rpc backend, so a coordinator on a live node with a stale
		// sibling is not a route either (same rule as the leader's /readyz).
		// Checked on every pick, before any cached engine: a gang that was
		// routable on its first request can lose a worker, or have one
		// drained, at any time after it.
		if !r.shardGroupRoutable(ctx, parts) {
			continue
		}
		// Set aside for THIS request: its coordinator could not be dialled a
		// moment ago (nextworker.go). The cooldown keeps later requests away.
		if skippedGang(ctx, gangKey(modelID, id)) {
			noteSkippedGang(ctx, modelID)
			continue
		}
		for _, s := range parts {
			if s.Role == "coordinator" && s.Status == "ready" {
				ready = append(ready, gangCandidate{gang: id, coord: s})
				break
			}
		}
	}
	if len(ready) == 0 {
		return nil, "", false
	}

	// The gangs are ranked by the order a worker is (rank.go): load, saturated
	// last, the prefix pin, the conversation's pin. A gang's load is what we
	// have sent to it plus what its head's node reports — the head is where a
	// request queues, so its node's signal is the gang's signal.
	//
	// A TIE goes round the gangs in turn, never to the first by name. With the
	// first always winning, one request at a time — most of a quiet endpoint's
	// life — was all served by one gang: a second gang held its cards and
	// carried nothing, and a canary gang was never sent the requests it is
	// judged on (run on a cluster, 2026-09-29: 209 of 209 to `g0`).
	cands := make([]candidate, len(ready))
	byKey := make(map[string]gangCandidate, len(ready))
	for i, c := range ready {
		cands[i] = c.candidate(modelID)
		byKey[cands[i].key] = c
	}
	order, sticky := r.rankCandidates(ctx, modelID, cands, &r.gangTurn)
	best := byKey[order[0].key]
	notePinOutcome(ctx, sticky, order[0].key)

	key := gangKey(modelID, best.gang)
	NoteGang(ctx, modelID, best.gang, best.coord.NodeID) // the attempt span names the head worker, not the key (T32.42); the route token names the gang (T28.8)
	r.mu.RLock()
	eng, cached := r.remotes[key]
	r.mu.RUnlock()
	if cached {
		return eng, key, true
	}
	// Engine-neutral gangs (R4): the coordinator row says which driver
	// fronts it (a vLLM Ray gang, or the llama.cpp RPC default).
	eng = engines.MustNew(CoordinatorEngine(best.coord), "http://"+best.coord.Address, "")
	r.mu.Lock()
	r.remotes[key] = eng
	r.mu.Unlock()
	return eng, key, true
}

// gangKey is the pseudo node id a gang's requests are accounted under. It keeps
// the "shard:" prefix, which is what exempts a gang from the per-node cooldown
// machinery — a gang is not a worker and must not be benched like one.
func gangKey(modelID, gangID string) string { return "shard:" + modelID + ":" + gangID }

// GangKey is gangKey for the leader, which publishes a gang's slot count
// under the key the picker counts the gang's requests by (ADR-091).
func GangKey(modelID, gangID string) string { return gangKey(modelID, gangID) }

// gangCandidate is a ready gang: its id and its head (coordinator) row.
type gangCandidate struct {
	gang  string
	coord store.Shard
}

// candidate is the gang as the shared ranking sees it: counted under its gang
// key, its load and its KV blocks read from its head's node — the head's
// worker is where a gang's engine publishes its cache events (T26.22). A head
// on the leader itself ("local") reports neither.
func (g gangCandidate) candidate(modelID string) candidate {
	c := candidate{key: gangKey(modelID, g.gang)}
	if g.coord.NodeID != "" && g.coord.NodeID != "local" {
		c.signal, c.blocks = g.coord.NodeID, g.coord.NodeID
	}
	return c
}

// gangRank scores one gang for the pick (candRank on its candidate). Caller
// holds r.mu.
func (r *Router) gangRank(gangID, modelID string, coord store.Shard) (saturated bool, score float64) {
	return r.candRank(gangCandidate{gang: gangID, coord: coord}.candidate(modelID))
}

// shardGroupRoutable reports whether every part of a gang sits on a worker
// that takes new work (store.Node.TakesNewWork: not drained, heartbeating
// within heartbeatMaxAge — 0 disables the age half, the legacy behaviour). A
// gang is one serving unit, so draining or losing the node of any part takes
// the whole gang out of rotation. Parts hosted by the leader itself ("local"
// / empty node id) always pass.
func (r *Router) shardGroupRoutable(ctx context.Context, shards []store.Shard) bool {
	now := time.Now()
	for _, sh := range shards {
		if sh.NodeID == "" || sh.NodeID == "local" {
			continue
		}
		n, err := r.store.Nodes().Get(ctx, sh.NodeID)
		if err != nil || n == nil {
			if r.heartbeatMaxAge <= 0 {
				continue // legacy: no liveness rule, an unknown node is not judged
			}
			return false
		}
		// The HEAD is judged in full — it is the process this router dials, so
		// an engine that stopped answering its own worker is exactly the news.
		// Every other part is judged on whether its worker is THERE: a gang
		// whose engine is one distributed process serves from rank 0 alone, so
		// the other ranks' engine probes can never succeed, and the full rule
		// took a healthy gang out of rotation about a minute after it formed
		// (store.Node.WhyNoPartWork; measured on the design-partner cell,
		// 2026-09-22). A part whose node went silent or was drained still stops
		// the gang, which is the rule that matters here.
		ok := n.TakesPartWork(r.heartbeatMaxAge, now)
		if sh.Role == "coordinator" {
			ok = n.TakesNewWork(r.heartbeatMaxAge, now)
		}
		if !ok {
			return false
		}
	}
	return true
}

// InvalidateModel drops any cached engine for the given model. Called by
// the orchestrator when shards are torn down so the next request rebuilds.
func (r *Router) InvalidateModel(modelID string) {
	prefix := "shard:" + modelID + ":"
	r.mu.Lock()
	// Every gang of the model, not just one: a create or a teardown can add,
	// move or remove any of them, and a cached engine for a gang that is gone
	// would keep being dialled.
	for key, eng := range r.remotes {
		if strings.HasPrefix(key, prefix) {
			delete(r.remotes, key)
			releaseEngine(eng)
		}
	}
	r.mu.Unlock()
}

// ForgetRemote drops only the cached client for a node, so the next request
// dials the address and signs with the token the node holds NOW. Called when a
// node registers again at a different address or with a different token: the
// client was built once from the first registration and keyed by node id, so
// a worker pod re-created under a pinned id (every worker with a certificate,
// R9.6) was dialled at its previous pod's IP until this leader restarted —
// found on the design-partner cell (2026-09-29, PLAN T7.3): a re-minted
// worker registered, heartbeat 200, and every chat was "worker could not be
// reached" at the old IP. The rest of the node's state (capabilities,
// cooldowns, in-flight counts) is the node's, not the process's, and stays.
func (r *Router) ForgetRemote(nodeID string) {
	if nodeID == "" {
		return
	}
	r.mu.Lock()
	if eng, ok := r.remotes[nodeID]; ok {
		releaseEngine(eng)
		delete(r.remotes, nodeID)
	}
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
	if eng, ok := r.remotes[nodeID]; ok {
		releaseEngine(eng)
	}
	delete(r.remotes, nodeID)
	delete(r.caps, nodeID)
	delete(r.staleWarned, nodeID)
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
// worker's address) or builds + caches one. engine is what the worker
// registered it serves through; the client is labelled with it, so an error a
// caller reads names the engine behind the worker — "worker" when the worker
// did not say — never the driver the leader happens to speak to it with.
//
// Concurrency: holds the write lock for the entire check-and-create so
// two concurrent calls for the same nodeID can't each construct a fresh
// engine and have one silently overwrite the other. The window is short
// (constructing a driver is a small struct alloc, no I/O) so write-lock for
// the duration is fine.
func (r *Router) getOrCreateRemote(nodeID, address, token, engine string) engines.Engine {
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
	// A worker is not a plain OpenAI server, though: it authenticates the leader
	// as the NODE, so the client signs every request rather than sending the
	// token as a bearer (engines.NodeSigned). Without that a worker configured
	// HMAC-only answers 401 to inference — every other leader→worker call
	// signed, and the request path did not.
	eng := engines.MustNew("vllm", endpoint, token)
	if signer, ok := eng.(engines.NodeSigned); ok {
		signer.SignAsNode(nodeID, token)
	}
	// The worker's own driver sends the salt to its engine, or does not; this
	// hop carries it only to a worker whose registered engine takes one, so
	// a worker that registered none — or an older one that would pass the key
	// on verbatim — is sent none (T26.8).
	if c, ok := eng.(engines.CacheSalted); ok {
		c.TakeCacheSalt(engines.TakesCacheSalt(engine))
	}
	if engine == "" {
		engine = "worker"
	}
	if l, ok := eng.(engines.Labeled); ok {
		l.LabelAs(engine)
	}
	r.remotes[nodeID] = eng
	return eng
}

func (r *Router) incInflight(nodeID, model string) {
	r.mu.Lock()
	r.inflight[nodeID]++
	r.inflightDim[nodeID+"|"+model]++
	r.noteSentLocked(nodeID, time.Now())
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

// CoordinatorEngine reads the driver name a shard coordinator row records in
// its config ({"engine":"vllm"}); llama.cpp when it records none. Exported
// because the leader samples that same process for its pressure
// (controlplane/loadstats.go) and must dial it with the driver the router uses.
func CoordinatorEngine(s store.Shard) string {
	if s.ConfigJSON != "" {
		var cfg struct {
			Engine string `json:"engine"`
		}
		if json.Unmarshal([]byte(s.ConfigJSON), &cfg) == nil && cfg.Engine != "" {
			return cfg.Engine
		}
	}
	return "llamacpp"
}

// CoordinatorKVLayout reads the KV layout a llama.cpp coordinator row records
// in its config ({"kv_unified":true}); known = false when it records none. The
// leader tells its sampling driver, which cannot read llama-server's pool
// without it (PLAN T23.13).
func CoordinatorKVLayout(s store.Shard) (unified, known bool) {
	if s.ConfigJSON == "" {
		return false, false
	}
	var cfg struct {
		KVUnified *bool `json:"kv_unified"`
	}
	if json.Unmarshal([]byte(s.ConfigJSON), &cfg) != nil || cfg.KVUnified == nil {
		return false, false
	}
	return *cfg.KVUnified, true
}
