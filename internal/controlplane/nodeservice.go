package controlplane

// NodeService (E1): the node lifecycle — register, heartbeat (placements +
// load + sleep state), drain, remove — as plain methods with typed errors.
// The HTTP handlers in nodes.go only decode, call, and encode; tests can
// drive the rules without a request.

import (
	"context"
	"errors"
	"time"

	"github.com/opod-io/opod-sdk/nodeapi"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

var (
	// ErrNodeBoundToOtherKey: a node-scope key presented an id another key owns.
	ErrNodeBoundToOtherKey = errors.New("node is bound to a different key")
	// ErrUnknownNode: heartbeat / drain for an id the leader has never seen.
	ErrUnknownNode = errors.New("unknown node — register first")
	// ErrLeaderNotDrainable: the leader's own "local" row. The router serves
	// from the local engine before it looks at any worker, so a drain there
	// would be a state nothing honours.
	ErrLeaderNotDrainable = errors.New("the leader's own node cannot be drained — unload its models or stop the leader instead")
)

// RegisterRequest is the worker's registration body.
type RegisterRequest struct {
	ID           string `json:"id"`
	Hostname     string `json:"hostname"`
	OS           string `json:"os"`
	Arch         string `json:"arch"`
	RAMGB        int    `json:"ram_gb"`
	Address      string `json:"address"`
	HardwareJSON string `json:"hardware_json"`
	BootID       string `json:"boot_id"` // the worker process (R10.1); "" = a worker that predates the field
}

// HeartbeatRequest is the worker's periodic ping.
type HeartbeatRequest struct {
	ID           string              `json:"id"`
	LoadedModels []string            `json:"loaded_models"`
	Load         *engines.EngineLoad `json:"load"`     // optional engine load sample (build item 14)
	Sleeping     bool                `json:"sleeping"` // sleep tier (build item 13): the engine sleeps, its placements are not routable
	BootID       string              `json:"boot_id"`  // the worker process (R10.1)
	// ResidentModels: which of LoadedModels are in the engine's memory right
	// now, from an engine that keeps installed models and loads on request
	// (Ollama). nil = the worker did not say, and everything loaded is taken
	// as resident; an empty list = nothing is in memory.
	ResidentModels *[]string `json:"resident_models"`
	// Engine (feature "engine_liveness") is what the worker's engine PROCESS is
	// doing, as the worker sees it — the only party that can say. A pod can be
	// Running while the engine inside it crash-loops, and to everything
	// counting workers that pod is capacity. nil = an older worker, or one
	// that launched no engine of ours: no statement, never "healthy".
	Engine *nodeapi.EngineState `json:"engine"`
}

// Caller is who is calling: admin keys pass every binding; a node key owns
// the ids it registered first.
type Caller struct {
	Admin bool
	KeyID string
}

// Register upserts the node row. First-use binding: the key that first
// registers a node id owns it; a node-scope key presenting a different id
// is refused, so one leaked node token cannot impersonate every node.
// bearer is the presented token, kept on the row so the leader can call the
// worker back (trusted network; HMAC signs the calls).
func (s *Server) RegisterNode(ctx context.Context, req RegisterRequest, caller Caller, bearer string) (store.Node, error) {
	existing, err := s.store.Nodes().Get(ctx, req.ID)
	if err != nil {
		return store.Node{}, err
	}
	boundKeyID := caller.KeyID
	if existing != nil && existing.BoundKeyID != "" {
		if !caller.Admin && caller.KeyID != existing.BoundKeyID {
			return store.Node{}, ErrNodeBoundToOtherKey
		}
		boundKeyID = existing.BoundKeyID // an admin re-register never re-owns the node
	}
	n := store.Node{
		ID: req.ID, Hostname: req.Hostname, OS: req.OS, Arch: req.Arch, RAMGB: req.RAMGB, Address: req.Address,
		WorkerToken: bearer, BoundKeyID: boundKeyID, HardwareJSON: req.HardwareJSON, LastHeartbeat: time.Now(), State: store.NodeStateReady,
		BootID: req.BootID,
	}
	if existing != nil && existing.Draining() {
		// A drain is the operator's word about the node id, not about one
		// process: a worker that restarts and registers again stays out of
		// rotation until it is undrained.
		n.State = store.NodeStateDraining
	}
	if err := s.store.Nodes().Upsert(ctx, n); err != nil {
		return store.Node{}, err
	}
	if existing != nil {
		switch incarnation(existing.BootID, req.BootID) {
		case incarnationNew:
			// A different process behind the same node id (R10.1): every row
			// keyed to the old one is gone with it — no process-list answer
			// needed.
			s.reincarnated(ctx, n, existing.BootID)
		case incarnationUnknown:
			// A worker that predates boot ids: what was recorded as running
			// on it is checked against its own process list on the next
			// heartbeat (its agent is listening by then).
			s.reconcileNodes.Store(n.ID, struct{}{})
		}
	}
	s.record("node.registered", n.ID, map[string]any{"hostname": n.Hostname, "address": n.Address, "again": existing != nil, "boot_id": n.BootID})
	return n, nil
}

// incarnation compares the boot id a row holds with the one a request
// carries.
type incarnationKind int

const (
	incarnationSame    incarnationKind = iota // the same process
	incarnationNew                            // both known, different: a new process
	incarnationUnknown                        // a side has no boot id (a worker from before the field): only a process-list check can tell
)

func incarnation(recorded, presented string) incarnationKind {
	switch {
	case recorded != "" && presented != "" && recorded != presented:
		return incarnationNew
	case recorded == "" || presented == "":
		return incarnationUnknown
	}
	return incarnationSame
}

// reincarnated drops what the leader recorded on a node's previous process:
// its placements now (the new process reports its own on the next
// heartbeat) and every shard group with a part on it — the part's process
// died with the old incarnation, and a row that still read "ready" would
// route a coordinator that no longer answers. The stop calls of the shard
// teardown go to the workers off the request path.
func (s *Server) reincarnated(ctx context.Context, n store.Node, previous string) {
	if err := s.store.Placements().ReplaceForNode(ctx, n.ID, nil); err != nil {
		s.log.Warn("placements of the previous incarnation not cleared", "node", n.ID, "err", err)
	}
	s.record("node.reincarnated", n.ID, map[string]any{"boot_id": n.BootID, "previous": previous})
	if s.orch == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		removed, err := s.orch.RemoveShardsOn(ctx, n.ID)
		if err != nil {
			s.log.Warn("shard rows of the previous incarnation not removed — the process-list check retries on the next heartbeat", "node", n.ID, "err", err)
			s.reconcileNodes.Store(n.ID, struct{}{})
			return
		}
		for _, m := range removed {
			s.log.Warn("shard group removed: its part died with the worker's previous incarnation", "model", m, "node", n.ID)
			s.record("shard.stale", m, map[string]any{"node": n.ID, "reason": "worker process changed (boot id); the recorded part is gone"})
		}
	}()
}

// reconcileShardsOn runs the orchestrator's stale-part check for a node
// that registered again (see RegisterNode), off the heartbeat's path.
func (s *Server) reconcileShardsOn(n store.Node) {
	if s.orch == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	removed, err := s.orch.ReconcileNode(ctx, n)
	if err != nil {
		s.log.Warn("stale shard check failed — will retry on the next register", "node", n.ID, "err", err)
		s.reconcileNodes.Store(n.ID, struct{}{})
		return
	}
	for _, m := range removed {
		s.log.Warn("shard group removed: its part died with the worker's previous incarnation", "model", m, "node", n.ID)
		s.record("shard.stale", m, map[string]any{"node": n.ID, "reason": "worker registered again; the recorded process is not running"})
	}
}

// HeartbeatNode refreshes liveness, keeps the engine's load sample, and
// reconciles the node's placements with what the worker reports loaded —
// engine-native names mapped back to catalog ids, one row per (node,
// model), status "sleeping" while the engine sleeps. Residency changes are
// recorded as model.loaded / model.unloaded.
//
// A heartbeat whose loaded_models is null carries no report and changes no
// row (below). A heartbeat reports residency, not routability: a placement the leader
// marked draining (Placements().SetStatus) stays draining for as long as the
// worker keeps reporting the model (ReplaceForNode carries the mark), and
// goes like any other row when the model leaves the report.
func (s *Server) HeartbeatNode(ctx context.Context, req HeartbeatRequest, caller Caller) error {
	n, err := s.store.Nodes().Get(ctx, req.ID)
	if err != nil {
		return err
	}
	if n == nil {
		return ErrUnknownNode
	}
	if n.BoundKeyID != "" && !caller.Admin && caller.KeyID != n.BoundKeyID {
		return ErrNodeBoundToOtherKey
	}
	if req.Engine != nil {
		s.nodeEngine.Store(req.ID, nodeEngineSample{EngineState: *req.Engine, at: time.Now()})
	}
	if req.Load != nil {
		s.nodeLoad.Store(req.ID, nodeLoadSample{EngineLoad: *req.Load, at: time.Now()})
	}
	if incarnation(n.BootID, req.BootID) == incarnationNew {
		// The process behind the id changed between heartbeats (a container
		// restarted in place: same pod, same address — the case no address
		// heuristic catches). R10.1.
		previous := n.BootID
		n.BootID = req.BootID
		s.reincarnated(ctx, *n, previous)
	} else if n.BootID == "" && req.BootID != "" {
		n.BootID = req.BootID // a row from before the field: adopt the process it now names
	}
	if _, again := s.reconcileNodes.LoadAndDelete(req.ID); again {
		go s.reconcileShardsOn(*n)
	}
	// A targeted write, never the row read above: a drain (or undrain) that
	// lands between that read and this write must not be undone by it.
	if err := s.store.Nodes().Heartbeat(ctx, n.ID, time.Now(), n.BootID); err != nil {
		return err
	}
	// A NULL loaded_models is "no report": the worker's engine did not answer
	// it in time, so this heartbeat says the worker is alive and nothing about
	// what it serves. The node's rows are left exactly as they are — statuses,
	// the leader's draining and released marks, cold flags — because replacing
	// them with nothing would delete the marks and the next real report would
	// recreate the rows `ready`: one slow engine tick would put a draining
	// source back in rotation. An EMPTY list still means "nothing is loaded"
	// and clears the rows. A worker whose engine never answers again does not
	// keep routable rows for ever: the row records since when
	// (EngineSilentSince) and the one live rule takes the node out of rotation
	// past its bound (store.Node.WhyNoNewWork), marks untouched.
	reported := req.LoadedModels != nil
	if reported != n.EngineSilentSince.IsZero() { // only a change is written
		if err := s.store.Nodes().NoteEngineReport(ctx, n.ID, reported, time.Now()); err != nil {
			s.log.Warn("engine report state not recorded", "node", n.ID, "err", err)
		}
	}
	// The journal says when the silence took the node out of rotation, once,
	// and when a report brought it back — not every slow tick.
	if !reported {
		if n.EngineSilent(s.heartbeatMaxAge(), time.Now()) {
			if _, told := s.engineSilentTold.LoadOrStore(n.ID, struct{}{}); !told {
				s.record("node.engine_silent", n.ID, map[string]any{"since": n.EngineSilentSince.UTC().Format(time.RFC3339)})
			}
		}
		return nil
	}
	if _, told := s.engineSilentTold.LoadAndDelete(n.ID); told {
		s.record("node.engine_reporting", n.ID, map[string]any{"silent_for_s": int(time.Since(n.EngineSilentSince).Seconds())})
	}
	status := "ready"
	if req.Sleeping {
		status = PlacementSleeping
	}
	var resident map[string]bool // nil = not reported
	if req.ResidentModels != nil {
		resident = make(map[string]bool, len(*req.ResidentModels))
		for _, m := range *req.ResidentModels {
			resident[s.catalogIDForNative(m)] = true
		}
	}
	placements := make([]store.Placement, 0, len(req.LoadedModels))
	seen := make(map[string]bool, len(req.LoadedModels))
	for _, m := range req.LoadedModels {
		id := s.catalogIDForNative(m)
		if seen[id] {
			continue
		}
		seen[id] = true
		// Installed and not in memory is still a routable row — the engine loads
		// it on the first request — but it holds no memory (Placement.Cold).
		placements = append(placements, store.Placement{NodeID: req.ID, ModelID: id, Status: status, LastSeen: time.Now(),
			Cold: resident != nil && !resident[id]})
	}
	prev := map[string]bool{}
	if old, err := s.store.Placements().GetByNode(ctx, req.ID); err == nil {
		for _, p := range old {
			prev[p.ModelID] = true
		}
	}
	if err := s.store.Placements().ReplaceForNode(ctx, req.ID, placements); err != nil {
		s.log.Warn("placements replace failed", "node", req.ID, "err", err)
		return nil // liveness was refreshed; the next heartbeat retries the rows
	}
	cur := map[string]bool{}
	for _, p := range placements {
		cur[p.ModelID] = true
		if !prev[p.ModelID] {
			s.record("model.loaded", p.ModelID, map[string]any{"node": req.ID})
		}
	}
	for m := range prev {
		if !cur[m] {
			s.record("model.unloaded", m, map[string]any{"node": req.ID})
		}
	}
	return nil
}

// DrainNode takes the node out of rotation: the router stops choosing it for
// new requests (router/pick.go, hedge.go) and the shard pickers stop placing
// parts on it; what is in flight on it finishes. The state survives
// heartbeats and a re-register; UndrainNode is the only way back.
func (s *Server) DrainNode(ctx context.Context, id string) error {
	if id == "local" {
		return ErrLeaderNotDrainable
	}
	return s.setNodeState(ctx, id, store.NodeStateDraining, "node.drained")
}

// UndrainNode puts a drained node back in rotation. The router reads the
// state on every pick, so the next request may already land on it.
func (s *Server) UndrainNode(ctx context.Context, id string) error {
	return s.setNodeState(ctx, id, store.NodeStateReady, "node.undrained")
}

func (s *Server) setNodeState(ctx context.Context, id, state, event string) error {
	found, err := s.store.Nodes().SetState(ctx, id, state)
	if err != nil {
		return err
	}
	if !found {
		return ErrUnknownNode
	}
	s.record(event, id, nil)
	return nil
}

// RemoveNode forgets the node, its placements and the router's cached
// engine for it, so in-flight routing stops picking it immediately.
func (s *Server) RemoveNode(ctx context.Context, id string) error {
	if err := s.store.Nodes().Delete(ctx, id); err != nil {
		return err
	}
	if ps, _ := s.store.Placements().GetByNode(ctx, id); ps != nil {
		for _, p := range ps {
			_ = s.store.Placements().Delete(ctx, p.NodeID, p.ModelID)
		}
	}
	s.router.InvalidateNode(id)
	s.record("node.removed", id, nil)
	return nil
}

func callerFrom(ctx context.Context) Caller {
	c := Caller{Admin: auth.ScopeFrom(ctx) == "admin"}
	if k := auth.KeyFrom(ctx); k != nil {
		c.KeyID = k.ID
	}
	return c
}
