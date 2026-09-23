package store

import (
	"fmt"
	"time"
)

// One rule for "does this node take new work?", asked wherever the leader
// decides or reports it: the router's pick (plain, revision groups, hedged,
// shard gangs), /readyz and the waking 503, /loadz `workers`, GET /v1/models,
// the shard pickers (scheduler.WorkerFor adds "has an address, is not the
// leader's own row") and `opod node ls`. It is derived from the row, never
// stored: a returning heartbeat, a returning engine report or an undrain
// makes the node live again with no reconciliation step.

// NodeStateLost is the derived state of a node believed ready whose
// heartbeats stopped. It is never persisted.
const NodeStateLost = "lost"

// DefaultHeartbeatMaxAge bounds a worker's heartbeat age for every caller
// that must have a bound even when the router's own check is switched off
// (router.heartbeat_max_age_seconds = 0).
const DefaultHeartbeatMaxAge = 60 * time.Second

// HeartbeatBound is the leader's liveness bound for a configured
// router.heartbeat_max_age_seconds: that many seconds, or
// DefaultHeartbeatMaxAge when the router's own check is off (0).
func HeartbeatBound(seconds int) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return DefaultHeartbeatMaxAge
}

// NodeStateEngineSilent is the derived state of a worker that heartbeats but
// whose engine has not answered it for longer than the bound: alive, and
// nothing says what it serves. Never persisted.
const NodeStateEngineSilent = "engine-silent"

// EngineSilent reports whether the worker's heartbeats have carried no
// engine report for longer than bound. The worker asks its engine on every
// heartbeat (every 5 s, waiting 2 s): one missed answer is a slow tick and
// changes nothing; the bound is the patience a worker that stops heartbeating
// gets — by then a dozen probes in a row went unanswered. bound <= 0 =
// DefaultHeartbeatMaxAge: unlike heartbeat age this rule has no off switch,
// because a heartbeating worker never turns "lost" on its own.
func (n Node) EngineSilent(bound time.Duration, now time.Time) bool {
	if n.EngineSilentSince.IsZero() {
		return false
	}
	if bound <= 0 {
		bound = DefaultHeartbeatMaxAge
	}
	return now.Sub(n.EngineSilentSince) > bound
}

// Alive reports whether the node heartbeated within maxAge. The leader's own
// "local" row never heartbeats and is always alive; maxAge <= 0 means the
// caller applies no age rule.
func (n Node) Alive(maxAge time.Duration, now time.Time) bool {
	if n.ID == "local" || maxAge <= 0 {
		return true
	}
	return now.Sub(n.LastHeartbeat) <= maxAge
}

// WhyNoNewWork is the rule: "" when the node takes new work — a request, a
// shard part — else the reason, fit for a message. A node takes new work
// when an operator has not drained it, its state is a serving one, it is
// alive, and its engine has not been silent past the same bound.
func (n Node) WhyNoNewWork(maxAge time.Duration, now time.Time) string {
	switch n.State {
	case NodeStateReady, NodeStateJoining, "":
	default: // draining, or a state this version does not serve from
		return "state " + n.State
	}
	if !n.Alive(maxAge, now) {
		return fmt.Sprintf("last heartbeat %s ago", now.Sub(n.LastHeartbeat).Round(time.Second))
	}
	if n.EngineSilent(maxAge, now) {
		return fmt.Sprintf("its engine has not answered it for %s", now.Sub(n.EngineSilentSince).Round(time.Second))
	}
	return ""
}

// WhyNoPartWork is WhyNoNewWork for a node that holds a GANG PART which is not
// the gang's head: "" when the part is still a reason to route to its gang,
// else why not.
//
// It differs in one rule, and the difference is the whole point. A gang whose
// engine is ONE distributed process — SGLang's launcher, vLLM over Ray — serves
// its API from rank 0 alone; the other ranks run the same binary, hold their
// share of the layers and answer no HTTP at all. So their worker's engine probe
// can never succeed, `EngineSilentSince` is set within a minute of the gang
// forming, and the engine-silence rule then declares a perfectly healthy part
// unable to take work — which takes the WHOLE gang out of rotation.
//
// Measured on the design-partner cell, 2026-09-22: a two-machine SGLang gang
// formed, answered the control plane's own probe in 713 ms, and was 502 by the
// next request. Judging a non-head part on an engine it was never meant to run
// is the defect; a part is judged on whether its WORKER is there, which is what
// a gang actually needs from it. The head keeps the full rule — that one really
// must serve.
func (n Node) WhyNoPartWork(maxAge time.Duration, now time.Time) string {
	switch n.State {
	case NodeStateReady, NodeStateJoining, "":
	default: // draining, or a state this version does not serve from
		return "state " + n.State
	}
	if !n.Alive(maxAge, now) {
		return fmt.Sprintf("last heartbeat %s ago", now.Sub(n.LastHeartbeat).Round(time.Second))
	}
	return ""
}

// TakesPartWork is WhyNoPartWork as a yes or no.
func (n Node) TakesPartWork(maxAge time.Duration, now time.Time) bool {
	return n.WhyNoPartWork(maxAge, now) == ""
}

// TakesNewWork is WhyNoNewWork as a yes or no.
func (n Node) TakesNewWork(maxAge time.Duration, now time.Time) bool {
	return n.WhyNoNewWork(maxAge, now) == ""
}

// LiveState is the state to show and act on: the stored one, except that a
// node believed ready (or still joining) whose heartbeats stopped is lost.
// Draining is kept as written — an operator's word outlives a silent node.
func (n Node) LiveState(maxAge time.Duration, now time.Time) string {
	if !n.Alive(maxAge, now) {
		return StateWhenSilent(n.State)
	}
	if n.EngineSilent(maxAge, now) && StateWhenSilent(n.State) == NodeStateLost {
		return NodeStateEngineSilent // a drain is kept as written here too
	}
	return n.State
}

// StateWhenSilent is what a stored state reads as once heartbeats stopped.
func StateWhenSilent(state string) string {
	switch state {
	case NodeStateReady, NodeStateJoining, "":
		return NodeStateLost
	}
	return state
}
