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
// stored: a returning heartbeat or an undrain makes the node live again with
// no reconciliation step.

// NodeStateLost is the derived state of a node believed ready whose
// heartbeats stopped. It is never persisted.
const NodeStateLost = "lost"

// DefaultHeartbeatMaxAge bounds a worker's heartbeat age for every caller
// that must have a bound even when the router's own check is switched off
// (router.heartbeat_max_age_seconds = 0).
const DefaultHeartbeatMaxAge = 60 * time.Second

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
// when an operator has not drained it, its state is a serving one, and it is
// alive.
func (n Node) WhyNoNewWork(maxAge time.Duration, now time.Time) string {
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

// TakesNewWork is WhyNoNewWork as a yes or no.
func (n Node) TakesNewWork(maxAge time.Duration, now time.Time) bool {
	return n.WhyNoNewWork(maxAge, now) == ""
}

// LiveState is the state to show and act on: the stored one, except that a
// node believed ready (or still joining) whose heartbeats stopped is lost.
// Draining is kept as written — an operator's word outlives a silent node.
func (n Node) LiveState(maxAge time.Duration, now time.Time) string {
	if n.Alive(maxAge, now) {
		return n.State
	}
	return StateWhenSilent(n.State)
}

// StateWhenSilent is what a stored state reads as once heartbeats stopped.
func StateWhenSilent(state string) string {
	switch state {
	case NodeStateReady, NodeStateJoining, "":
		return NodeStateLost
	}
	return state
}
