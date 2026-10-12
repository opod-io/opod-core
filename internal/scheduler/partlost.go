package scheduler

// A gang create ends the moment the leader knows one of its parts' workers is
// gone.
//
// A create waits for each part to leave "starting" (awaitWorkerReady), and an
// error from the worker during that wait is tolerated as a blip: a worker that
// is briefly unreachable must not cost a coordinator that is loading fine. But
// the leader often KNOWS the blip is not one — the worker behind the node id
// was replaced by a new process (its boot id changed: a recreated pod, a
// restarted container), or the node was removed. The part's process died with
// it, and polling it can only run out the formation budget.
//
// Measured on the design-partner cell (2026-10-11): a coordinator's worker pod
// died 17 s after the coordinator started, the leader logged "gang removed: its
// part died with the worker's previous incarnation" the same second, and the
// create polled the dead process for 7.5 more minutes — holding the gang's
// create guard, so every heal attempt was refused — until its caller's own
// deadline cancelled it with no reason at all.
//
// Two ways the create learns it, both from facts the leader already holds:
//
//   - a notification: the leader's reincarnation handling (RemoveShardsOn), a
//     stale-part removal (ReconcileNode) and a node removal (EndFormationsOn)
//     cancel every create forming a part on that node, through the create's
//     own context. This also ends an in-flight call to a dead pod address at
//     once rather than after the HTTP client's timeout.
//   - a check: when a poll fails, awaitWorkerReady reads the node's row again.
//     A row that is gone, or that carries a different boot id than the one the
//     part was started on, ends the wait. The same row with the same boot id
//     is a blip, and is still waited out.
//
// What this does NOT cover: a coordinator whose worker stays the same process
// and that neither opens its port nor exits (llama-server sitting in
// "starting") still waits for its ReadyTimeout.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/store"
)

// ErrPartLost marks a create that ended because a part's worker went away
// while the gang was forming. The create's parts are rolled back and its guard
// is released, so the gang can be created again at once.
var ErrPartLost = errors.New("a part's worker went away while the gang was forming")

// What happened to a part's worker, in the words the error carries.
const (
	lostReplaced = "was replaced by a new worker process (its boot id changed)"
	lostRemoved  = "was removed from the leader"
	lostProcess  = "no longer runs the gang's part (its process is gone)"
)

// PartLostError is the reason a create ended early: which gang, which node,
// what happened to it, and what the leader last heard from the process the
// create was waiting for.
type PartLostError struct {
	Model, Gang string
	Node        string // the node whose worker went away
	What        string // lostReplaced, lostRemoved or lostProcess

	// The process the create was waiting on when it ended, when it was waiting
	// on one: its id, the node it runs on (not always Node: a sibling part's
	// worker can be the one that went), and the last status the leader polled.
	Process, ProcessNode string
	Last                 *agent.ProcessInfo
	LastAt               time.Time
}

func (e *PartLostError) Unwrap() error { return ErrPartLost }

func (e *PartLostError) Error() string {
	var b strings.Builder
	gang := "the gang"
	if e.Gang != "" {
		gang = "gang " + e.Gang
	}
	if e.Model != "" {
		gang += " of " + e.Model
	}
	fmt.Fprintf(&b, "%s did not form: the worker behind node %s %s while the gang was forming", gang, e.Node, e.What)
	if e.Process != "" {
		switch {
		case e.Last == nil:
			fmt.Fprintf(&b, "; the worker reported no output for process %s on %s before it went (no status had been polled yet)", e.Process, e.ProcessNode)
		case e.Last.ExitErr != "":
			// The engine's own last lines (the supervisor's log tail rides
			// ExitErr), from a generation that exited before this one.
			fmt.Fprintf(&b, "; process %s on %s last reported %q after %d restart(s), with: %s",
				e.Process, e.ProcessNode, e.Last.Status, e.Last.Restarts, e.Last.ExitErr)
		default:
			fmt.Fprintf(&b, "; process %s on %s last reported %q %s before, and the worker reported no output for it before it went",
				e.Process, e.ProcessNode, e.Last.Status, e.since())
		}
	}
	b.WriteString(" — the create's parts were stopped and the gang can be created again now")
	return b.String()
}

func (e *PartLostError) since() string {
	if e.LastAt.IsZero() {
		return "some time"
	}
	return time.Since(e.LastAt).Round(time.Second).String()
}

// waitingOn is a copy of e that also says what the create was waiting for.
func (e *PartLostError) waitingOn(process, node string, last *agent.ProcessInfo, at time.Time) *PartLostError {
	c := *e
	c.Process, c.ProcessNode, c.Last, c.LastAt = process, node, last, at
	return &c
}

// partLostCause returns the PartLostError a create's context was cancelled
// with, or nil.
func partLostCause(ctx context.Context) *PartLostError {
	var pl *PartLostError
	if errors.As(context.Cause(ctx), &pl) {
		return pl
	}
	return nil
}

// formationSet is every gang create forming right now, with the nodes its
// parts are placed on, so news about a node can reach the creates using it.
type formationSet struct {
	mu   sync.Mutex
	live map[*formation]struct{}
}

type formation struct {
	model, gang string
	nodes       map[string]bool
	cancel      context.CancelCauseFunc
}

// begin registers a create of (model, gang) on workers and returns the context
// its formation work runs under, and the call that ends the registration.
// Register before any part starts: a node lost after that is a part lost.
func (s *formationSet) begin(ctx context.Context, model, gang string, workers []store.Node) (context.Context, func()) {
	fctx, cancel := context.WithCancelCause(ctx)
	f := &formation{model: model, gang: gang, nodes: map[string]bool{}, cancel: cancel}
	for _, w := range workers {
		f.nodes[w.ID] = true
	}
	s.mu.Lock()
	if s.live == nil {
		s.live = map[*formation]struct{}{}
	}
	s.live[f] = struct{}{}
	s.mu.Unlock()
	return fctx, func() {
		s.mu.Lock()
		delete(s.live, f)
		s.mu.Unlock()
		cancel(nil)
	}
}

// end cancels every create that match accepts, with a PartLostError naming
// node and what happened. Returns how many it ended.
func (s *formationSet) end(node, what string, match func(*formation) bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for f := range s.live {
		if match(f) {
			f.cancel(&PartLostError{Model: f.model, Gang: f.gang, Node: node, What: what})
			n++
		}
	}
	return n
}

// EndFormationsOn ends every gang create forming a part on the node, because
// the node was removed. The create rolls back what it started and returns
// ErrPartLost naming the node; its guard is released on return.
func (o *Orchestrator) EndFormationsOn(nodeID string) {
	o.endFormationsOn(nodeID, lostRemoved)
}

func (o *Orchestrator) endFormationsOn(nodeID, what string) {
	if n := o.formations.end(nodeID, what, func(f *formation) bool { return f.nodes[nodeID] }); n > 0 {
		o.Log.Warn("gang create ended: a part's worker went away while the gang was forming", "node", nodeID, "creates", n, "why", what)
	}
}

// endFormationOf ends the create forming one gang, whose part on nodeID is gone.
func (o *Orchestrator) endFormationOf(key store.GangKey, nodeID, what string) {
	if n := o.formations.end(nodeID, what, func(f *formation) bool { return f.model == key.Model && f.gang == key.Gang }); n > 0 {
		o.Log.Warn("gang create ended: a part's worker went away while the gang was forming", "model", key.Model, "gang", key.Gang, "node", nodeID, "why", what)
	}
}

// partGone reads the node's row again after a failed poll: nil when the row
// still names the process the part was started on (a blip — keep waiting),
// else what happened to it.
func (o *Orchestrator) partGone(ctx context.Context, node store.Node) (what string) {
	now, err := o.Store.Nodes().Get(ctx, node.ID)
	switch {
	case err != nil:
		return "" // the store says nothing: treat the poll error as a blip
	case now == nil:
		return lostRemoved
	case node.BootID != "" && now.BootID != "" && now.BootID != node.BootID:
		return lostReplaced
	}
	return ""
}

// ErrPartFailed marks a create that ended because one of its parts' processes
// gave up while the gang was forming: its supervisor reported "failed", or
// "crashloop" once its restarts ran out. Like ErrPartLost it is definite — the
// create's parts are rolled back and the gang will not form on its own.
var ErrPartFailed = errors.New("a part's process failed while the gang was forming")

// PartFailedError is the reason a create ended on a part that gave up: which
// gang, which process on which node, the status it reported and the engine's
// own last lines (the supervisor's log tail rides ExitErr).
type PartFailedError struct {
	Model, Gang   string
	Process, Node string
	Status        string // "failed" or "crashloop"
	Restarts      int
	ExitErr       string
}

func (e *PartFailedError) Unwrap() error { return ErrPartFailed }

func (e *PartFailedError) Error() string {
	var b strings.Builder
	if e.Gang != "" || e.Model != "" {
		gang := "the gang"
		if e.Gang != "" {
			gang = "gang " + e.Gang
		}
		if e.Model != "" {
			gang += " of " + e.Model
		}
		fmt.Fprintf(&b, "%s did not form: ", gang)
	}
	fmt.Fprintf(&b, "process %s on %s %s", e.Process, e.Node, e.Status)
	if e.Restarts > 0 {
		fmt.Fprintf(&b, " after %d restart(s)", e.Restarts)
	}
	if e.ExitErr != "" {
		fmt.Fprintf(&b, ": %s", e.ExitErr)
	}
	return b.String()
}

// withGang names the gang on a PartFailedError anywhere in err's chain; any
// other error is returned unchanged.
func withGang(err error, model, gang string) error {
	var pf *PartFailedError
	if !errors.As(err, &pf) {
		return err
	}
	c := *pf
	c.Model, c.Gang = model, gang
	return &c
}

// asPartLost turns a create's failure into the PartLostError that caused it,
// filled with the gang, when the create ended because a part was lost; else
// it returns err unchanged. The failure may carry the reason itself (the wait
// returned it) or only a cancelled call, with the reason on fctx.
func asPartLost(fctx context.Context, err error, model, gang string) error {
	if err == nil {
		return nil
	}
	var pl *PartLostError
	if !errors.As(err, &pl) {
		if pl = partLostCause(fctx); pl == nil {
			return err
		}
	}
	c := *pl
	c.Model, c.Gang = model, gang
	return &c
}
