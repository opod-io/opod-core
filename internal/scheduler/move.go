package scheduler

// Live model move: have another worker serve a whole (non-sharded) model
// instead of the one that serves it now, with no request failing and none
// meeting a model that is still loading.
//
// Weights never travel between engines. What a move does is OVERLAP: the
// target loads the model itself while the source keeps serving; only once the
// target is routable does the router stop choosing the source; what is in
// flight on the source finishes; then the source lets the model go. Until the
// flip nothing a client can see has changed, so every failure before it ends
// with the source serving and the target cleaned up.
//
// No KV cache is transferred: a conversation the source was serving has its
// next turn recomputed from the prompt on the target (slower first token, same
// answer).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// ErrMoveNotFound marks a move that names a node the leader does not know.
var ErrMoveNotFound = errors.New("not found")

// Move steps, in the order they happen; each is journalled by the caller as
// the event "model.move_<step>".
const (
	MoveStarted  = "started"  // admitted: nothing has been touched yet
	MoveLoaded   = "loaded"   // the target's placement is routable; both workers serve
	MoveFlipped  = "flipped"  // the source's placement is draining: new requests go elsewhere
	MoveDrained  = "drained"  // nothing is in flight on the source for the model (or the wait ran out)
	MoveFinished = "finished" // the source let the model go
	MoveAborted  = "aborted"  // stopped; Data["reason"] says why and Data["state"] what serves now
)

// Defaults of MoveOptions.
const (
	DefaultMoveReadyTimeout = 30 * time.Minute // a cold target downloads the weights inside this
	DefaultMoveDrainTimeout = 30 * time.Second // lifecycle.DefaultDrainTimeout: the local eviction's patience
	DefaultMoveSettle       = 20 * time.Second // a few heartbeats: how long the source's row is watched after the unload
)

// MoveOptions is one move and what the leader knows that the orchestrator
// does not.
type MoveOptions struct {
	From, To string // node id or hostname

	ReadyTimeout time.Duration // bound on "target routable"; 0 = DefaultMoveReadyTimeout
	DrainTimeout time.Duration // bound on "source idle"; 0 = DefaultMoveDrainTimeout
	Settle       time.Duration // 0 = DefaultMoveSettle
	Poll         time.Duration // 0 = 1s

	Catalog        []models.Entry // for the memory facts
	ReservePercent int            // placement.reserve_percent
	// Inflight is the router's per-(node, model) live request count, keyed
	// node + "|" + model (router.InflightByModel). nil = nothing is tracked,
	// and the drain step waits out its timeout instead of assuming zero.
	Inflight func() map[string]int
	// Step is told every step as it completes (the leader journals it).
	Step func(MoveStep)
}

// MoveStep is one completed step.
type MoveStep struct {
	Step string         `json:"step"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data,omitempty"`
}

// MoveResult is a finished move.
type MoveResult struct {
	Model string     `json:"model"`
	From  string     `json:"from"`
	To    string     `json:"to"`
	Steps []MoveStep `json:"steps"`
	// Note is set when the move finished with something the operator should
	// know (the source's engine keeps the weights installed).
	Note string `json:"note,omitempty"`
}

// moveGuard keeps two moves from working on the same thing at once: a model
// being moved, or a node that is the source or target of a running move
// (admission reads placement rows, and a second move would not see the first
// one's load until its row appears).
type moveGuard struct {
	mu   sync.Mutex
	busy map[string]string // "model:<id>" | "node:<id>" → the move holding it
}

func (g *moveGuard) claim(model, from, to string) (release func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.busy == nil {
		g.busy = map[string]string{}
	}
	keys := []string{"model:" + model, "node:" + from, "node:" + to}
	for _, k := range keys {
		if holder, taken := g.busy[k]; taken {
			return nil, fmt.Errorf("%w: a move is already running (%s) and involves %s; wait for it to end",
				ErrUnplaceable, holder, strings.SplitN(k, ":", 2)[1])
		}
	}
	what := fmt.Sprintf("%s: %s → %s", model, from, to)
	for _, k := range keys {
		g.busy[k] = what
	}
	return func() {
		g.mu.Lock()
		for _, k := range keys {
			delete(g.busy, k)
		}
		g.mu.Unlock()
	}, nil
}

// MoveModel runs the overlap sequence for a non-sharded model. Refusals wrap
// ErrUnplaceable (or ErrMoveNotFound for an unknown node) and are returned
// before anything is touched; any later error comes with an "aborted" step
// that says what serves now.
//
// ctx bounds the move but should NOT be a request's context: a client that
// hangs up after the flip must not leave the source draining with nobody to
// finish. The leader passes a context detached from the request.
func (o *Orchestrator) MoveModel(ctx context.Context, entry models.Entry, opt MoveOptions) (MoveResult, error) {
	opt = opt.withDefaults()
	from, to, already, err := o.admitMove(ctx, entry, opt)
	if err != nil {
		return MoveResult{}, err
	}
	release, err := o.moves.claim(entry.ID, from.ID, to.ID)
	if err != nil {
		return MoveResult{}, err
	}
	defer release()

	res := MoveResult{Model: entry.ID, From: from.ID, To: to.ID}
	step := func(name string, data map[string]any) {
		s := MoveStep{Step: name, At: time.Now().UTC(), Data: data}
		res.Steps = append(res.Steps, s)
		if opt.Step != nil {
			opt.Step(s)
		}
	}
	// abort ends a move that has not flipped: the source was never touched.
	abort := func(reason error, loadedHere bool) (MoveResult, error) {
		state := "the source still serves; the target was not touched"
		if loadedHere {
			state = "the source still serves; the target's copy was unloaded"
			if cleanupErr := o.cleanupTarget(entry, to); cleanupErr != nil {
				state = "the source still serves; the target's copy could NOT be unloaded (" + cleanupErr.Error() + ") — inspect the target"
			}
		}
		step(MoveAborted, map[string]any{"reason": reason.Error(), "state": state})
		return res, fmt.Errorf("move aborted, %s: %w", state, reason)
	}

	step(MoveStarted, map[string]any{"from": from.ID, "to": to.ID, "target_already_serves": already})

	// 2. load on the target — the source keeps serving, untouched.
	if !already {
		if err := o.callWorkerLoad(ctx, to, entry, false); err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				return abort(fmt.Errorf("load on %s: %w", to.ID, err), true)
			}
			// The load call outlived the leader's client timeout: the worker
			// may still be pulling weights. Readiness is what the next step
			// waits for, under its own bound.
			o.Log.Info("model move: the load call timed out at the client; waiting for the target's placement", "model", entry.ID, "node", to.ID)
		}
	}

	// 3. ready — the target's row, written by its own heartbeat, is routable.
	if err := o.awaitRoutable(ctx, entry.ID, to.ID, opt); err != nil {
		return abort(err, !already)
	}
	step(MoveLoaded, map[string]any{"node": to.ID})

	// 4. flip — the router stops choosing the source for this model. The mark
	// survives the source's heartbeats (store.PlacementDraining).
	if err := o.Store.Placements().SetStatus(ctx, from.ID, entry.ID, store.PlacementDraining); err != nil {
		return abort(fmt.Errorf("mark %s draining on %s: %w", entry.ID, from.ID, err), !already)
	}
	step(MoveFlipped, map[string]any{"node": from.ID})

	// From here the context is no longer allowed to stop the move: the source
	// is out of rotation and only finishing, or putting it back, is a state
	// worth leaving.
	ctx = context.WithoutCancel(ctx)

	// 5. drain — what is in flight on (source, model) runs to completion.
	left, timedOut := o.awaitIdle(ctx, from.ID, entry.ID, opt)
	if timedOut {
		o.Log.Warn("model move: drain timeout — unloading the source with requests still in flight", "model", entry.ID, "node", from.ID, "inflight", left)
	}
	step(MoveDrained, map[string]any{"node": from.ID, "inflight_left": left, "timed_out": timedOut})

	// 6. unload — the source lets the model go.
	unloadCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	ures, err := o.callWorkerUnload(unloadCtx, from, entry)
	cancel()
	if err != nil {
		// The source could not let go. Put it back in rotation: two workers
		// serving is a state that is true and harmless; a draining row over a
		// model nobody will unload is neither.
		state := "both workers serve the model: the source was put back in rotation"
		if backErr := o.Store.Placements().SetStatus(ctx, from.ID, entry.ID, "ready"); backErr != nil {
			state = "the target serves; the source's placement is still draining and its model still loaded — a leader restart lifts the mark"
		}
		step(MoveAborted, map[string]any{"reason": "unload on " + from.ID + ": " + err.Error(), "state": state})
		return res, fmt.Errorf("move aborted, %s: unload on %s: %w", state, from.ID, err)
	}
	res.Note = o.settleSource(ctx, entry.ID, from.ID, opt)
	step(MoveFinished, map[string]any{"from": from.ID, "to": to.ID, "unloaded": ures.Unloaded, "note": res.Note})
	return res, nil
}

func (opt MoveOptions) withDefaults() MoveOptions {
	if opt.ReadyTimeout <= 0 {
		opt.ReadyTimeout = DefaultMoveReadyTimeout
	}
	if opt.DrainTimeout <= 0 {
		opt.DrainTimeout = DefaultMoveDrainTimeout
	}
	if opt.Settle <= 0 {
		opt.Settle = DefaultMoveSettle
	}
	if opt.Poll <= 0 {
		opt.Poll = time.Second
	}
	return opt
}

// admitMove refuses, before anything is touched, every move that cannot
// work. already reports that the target serves the model now (a second
// replica): the load is then skipped — on a one-model engine a load would
// restart the very process that serves it.
func (o *Orchestrator) admitMove(ctx context.Context, entry models.Entry, opt MoveOptions) (from, to store.Node, already bool, err error) {
	refuse := func(format string, args ...any) (store.Node, store.Node, bool, error) {
		return store.Node{}, store.Node{}, false, fmt.Errorf("%w: "+format, append([]any{ErrUnplaceable}, args...)...)
	}
	nodes, err := o.Store.Nodes().List(ctx)
	if err != nil {
		return store.Node{}, store.Node{}, false, err
	}
	find := func(name string) (store.Node, bool) {
		for _, n := range nodes {
			if n.ID == name || (n.Hostname != "" && n.Hostname == name) {
				return n, true
			}
		}
		return store.Node{}, false
	}
	var ok bool
	if from, ok = find(opt.From); !ok {
		return store.Node{}, store.Node{}, false, fmt.Errorf("%w: node %q (--from)", ErrMoveNotFound, opt.From)
	}
	if to, ok = find(opt.To); !ok {
		return store.Node{}, store.Node{}, false, fmt.Errorf("%w: node %q (--to)", ErrMoveNotFound, opt.To)
	}
	if from.ID == to.ID {
		return refuse("the source and the target are the same node (%s)", from.ID)
	}
	if entry.Sharding.Required {
		return refuse("%s is a sharded model; a gang is not moved part by part — rebuild it where you want it with `opod shard create`", entry.ID)
	}
	if shards, err := o.Store.Shards().GetByModel(ctx, entry.ID); err != nil {
		return store.Node{}, store.Node{}, false, err
	} else if len(shards) > 0 {
		return refuse("%s is served by a sharded placement; a gang is not moved part by part — rebuild it where you want it with `opod shard create`", entry.ID)
	}
	if from.ID == "local" || from.Address == "" {
		return refuse("the source %s is not a worker (the leader's own engine is unloaded with `opod model unload`)", from.ID)
	}

	// The source holds it, routably, and nothing of it would be left behind.
	onFrom, err := o.Store.Placements().GetByNode(ctx, from.ID)
	if err != nil {
		return store.Node{}, store.Node{}, false, err
	}
	held, adapters := "", []string{}
	for _, p := range onFrom {
		switch {
		case p.ModelID == entry.ID:
			held = p.Status
			if held == "" {
				held = "ready"
			}
		case strings.HasPrefix(p.ModelID, entry.ID+":") && p.Status != store.PlacementReleased:
			adapters = append(adapters, p.ModelID)
		}
	}
	switch held {
	case "":
		return refuse("node %s does not hold %s", from.ID, entry.ID)
	case "ready", "sleeping":
	default:
		return refuse("node %s holds %s as %q, not as a serving placement", from.ID, entry.ID, held)
	}
	if len(adapters) > 0 {
		return refuse("node %s serves adapter(s) of %s (%s); a move does not carry adapters — drop them first (DELETE /admin/v1/adapters/{name}) and add them again once the target serves the base",
			from.ID, entry.ID, strings.Join(adapters, ", "))
	}

	// The target takes new work …
	now := time.Now()
	if ok, why := WorkerFor(to, o.HeartbeatMaxAge, now); !ok {
		return refuse("the target %s does not take new work (%s)", to.ID, why)
	}
	onTo, err := o.Store.Placements().GetByNode(ctx, to.ID)
	if err != nil {
		return store.Node{}, store.Node{}, false, err
	}
	for _, p := range onTo {
		if p.ModelID == entry.ID && (p.Status == "" || p.Status == "ready") && !p.Cold {
			already = true
		}
	}
	if already {
		return from, to, true, nil
	}
	// … would not lose a model to the load …
	if err := LoadWouldReplace(to, onTo, entry.ID); err != nil {
		return store.Node{}, store.Node{}, false, err
	}
	// … and can hold the model beside what it already has: the overlap means
	// it is resident twice, once on each machine. The facts are the
	// shard-count picker's.
	if need := ShardNeedBytes(entry); need > 0 {
		facts, err := WorkerMemoryFacts(ctx, o.Store, opt.Catalog, "", opt.ReservePercent, o.HeartbeatMaxAge, now)
		if err != nil {
			return store.Node{}, store.Node{}, false, err
		}
		for _, w := range facts {
			if w.NodeID == to.ID && w.FreeBytes < need {
				return refuse("the target %s cannot hold %s beside what it has: the model needs %s and %s has %s free (%s of %s in use)",
					to.ID, entry.ID, gb(need), to.ID, gb(w.FreeBytes), gb(w.ResidentBytes), gb(w.CapacityBytes))
			}
		}
	}
	return from, to, false, nil
}

// awaitRoutable waits until the target's own heartbeat has written a row the
// router would route to: status ready, in memory (not cold), on a node that
// still takes new work.
func (o *Orchestrator) awaitRoutable(ctx context.Context, model, nodeID string, opt MoveOptions) error {
	deadline := time.Now().Add(opt.ReadyTimeout)
	for {
		if n, err := o.Store.Nodes().Get(ctx, nodeID); err == nil && n != nil {
			if ok, _ := WorkerFor(*n, o.HeartbeatMaxAge, time.Now()); ok {
				if ps, err := o.Store.Placements().GetByNode(ctx, nodeID); err == nil {
					for _, p := range ps {
						if p.ModelID == model && (p.Status == "" || p.Status == "ready") && !p.Cold {
							return nil
						}
					}
				}
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the target %s did not serve %s within %s", nodeID, model, opt.ReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped while waiting for the target %s to serve %s: %w", nodeID, model, ctx.Err())
		case <-time.After(opt.Poll):
		}
	}
}

// awaitIdle waits for the router's in-flight count of (node, model) to reach
// zero, bounded by the drain timeout. It returns what was left and whether
// the wait ran out.
func (o *Orchestrator) awaitIdle(ctx context.Context, nodeID, model string, opt MoveOptions) (left int, timedOut bool) {
	deadline := time.Now().Add(opt.DrainTimeout)
	key := nodeID + "|" + model
	for {
		if opt.Inflight != nil {
			if left = opt.Inflight()[key]; left == 0 {
				return 0, false
			}
		}
		if time.Now().After(deadline) {
			return left, true
		}
		select {
		case <-ctx.Done():
			return left, true
		case <-time.After(opt.Poll):
		}
	}
}

// cleanupTarget unloads the copy a failed move loaded on its target, on a
// context of its own — the move's may be the reason it failed.
func (o *Orchestrator) cleanupTarget(entry models.Entry, to store.Node) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	_, err := o.callWorkerUnload(ctx, to, entry)
	return err
}

// settleSource watches the source's row after the unload. A worker that
// launched its engine for the model stops listing it and the row goes with
// the next heartbeat — nothing to do. An engine that keeps the weights
// installed keeps listing it: once the worker reports it as not in memory the
// row is marked released, so the router stays away from it across leader
// restarts, until someone loads the model there again or removes the weights.
// The returned note is for the operator; "" when the row simply went.
func (o *Orchestrator) settleSource(ctx context.Context, model, nodeID string, opt MoveOptions) string {
	deadline := time.Now().Add(opt.Settle)
	for {
		var row *store.Placement
		if ps, err := o.Store.Placements().GetByNode(ctx, nodeID); err == nil {
			for i := range ps {
				if ps[i].ModelID == model {
					row = &ps[i]
				}
			}
		}
		switch {
		case row == nil:
			return ""
		case row.Cold:
			if err := o.Store.Placements().SetStatus(ctx, nodeID, model, store.PlacementReleased); err != nil {
				return fmt.Sprintf("%s keeps the weights installed and its placement could not be marked released (%v); it stays draining until the leader restarts", nodeID, err)
			}
			return fmt.Sprintf("%s keeps the weights of %s installed (its engine does not delete on unload); the placement is marked released — not routed to — until the model is loaded there again or the weights are removed", nodeID, model)
		case time.Now().After(deadline):
			return fmt.Sprintf("%s still lists %s after the unload and does not say whether it is in memory; its placement stays draining — not routed to — until this leader restarts", nodeID, model)
		}
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(opt.Poll):
		}
	}
}
