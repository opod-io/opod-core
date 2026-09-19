package store

import (
	"testing"
	"time"
)

// The one rule, as a table: who takes new work, why not, and what is shown.
func TestNodeTakesNewWork(t *testing.T) {
	now := time.Now()
	fresh, silent := now.Add(-5*time.Second), now.Add(-10*time.Minute)
	for _, c := range []struct {
		name      string
		n         Node
		maxAge    time.Duration
		takes     bool
		why, show string
	}{
		{"ready and heartbeating", Node{ID: "w", State: NodeStateReady, LastHeartbeat: fresh}, time.Minute, true, "", "ready"},
		{"still joining", Node{ID: "w", State: NodeStateJoining, LastHeartbeat: fresh}, time.Minute, true, "", "joining"},
		{"a row from before states", Node{ID: "w", LastHeartbeat: fresh}, time.Minute, true, "", ""},
		{"drained", Node{ID: "w", State: NodeStateDraining, LastHeartbeat: fresh}, time.Minute, false, "state draining", "draining"},
		{"heartbeats stopped", Node{ID: "w", State: NodeStateReady, LastHeartbeat: silent}, time.Minute, false, "last heartbeat 10m0s ago", NodeStateLost},
		{"drained and silent stays drained", Node{ID: "w", State: NodeStateDraining, LastHeartbeat: silent}, time.Minute, false, "state draining", "draining"},
		{"no age rule", Node{ID: "w", State: NodeStateReady, LastHeartbeat: silent}, 0, true, "", "ready"},
		{"the leader's own row never heartbeats", Node{ID: "local", State: NodeStateReady, LastHeartbeat: silent}, time.Minute, true, "", "ready"},
		{"one slow engine tick", Node{ID: "w", State: NodeStateReady, LastHeartbeat: fresh, EngineSilentSince: fresh}, time.Minute, true, "", "ready"},
		{"its engine stopped answering", Node{ID: "w", State: NodeStateReady, LastHeartbeat: fresh, EngineSilentSince: silent}, time.Minute, false, "its engine has not answered it for 10m0s", NodeStateEngineSilent},
		{"engine silence has no off switch", Node{ID: "w", State: NodeStateReady, LastHeartbeat: fresh, EngineSilentSince: silent}, 0, false, "its engine has not answered it for 10m0s", NodeStateEngineSilent},
		{"drained with a silent engine stays drained", Node{ID: "w", State: NodeStateDraining, LastHeartbeat: fresh, EngineSilentSince: silent}, time.Minute, false, "state draining", "draining"},
		{"lost outranks a silent engine", Node{ID: "w", State: NodeStateReady, LastHeartbeat: silent, EngineSilentSince: silent}, time.Minute, false, "last heartbeat 10m0s ago", NodeStateLost},
		{"a state this version does not serve from", Node{ID: "w", State: "retired", LastHeartbeat: fresh}, time.Minute, false, "state retired", "retired"},
	} {
		if got := c.n.TakesNewWork(c.maxAge, now); got != c.takes {
			t.Errorf("%s: TakesNewWork = %v, want %v", c.name, got, c.takes)
		}
		if got := c.n.WhyNoNewWork(c.maxAge, now); got != c.why {
			t.Errorf("%s: WhyNoNewWork = %q, want %q", c.name, got, c.why)
		}
		if got := c.n.LiveState(c.maxAge, now); got != c.show {
			t.Errorf("%s: LiveState = %q, want %q", c.name, got, c.show)
		}
	}
}
