package scheduler

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The drain wait ends at zero in flight, or at its bound with what is left;
// with no counter at all it waits the bound out rather than assume zero.
func TestMoveAwaitIdle(t *testing.T) {
	o := &Orchestrator{}
	opt := MoveOptions{DrainTimeout: 200 * time.Millisecond, Poll: 5 * time.Millisecond}
	var calls atomic.Int64
	opt.Inflight = func() map[string]int {
		if calls.Add(1) < 4 {
			return map[string]int{"w1|m": 2, "w1|other": 9}
		}
		return map[string]int{"w1|other": 9} // another model on the node is not waited for
	}
	if left, timedOut := o.awaitIdle(context.Background(), "w1", "m", opt); left != 0 || timedOut {
		t.Errorf("idle: left %d, timed out %v", left, timedOut)
	}
	opt.Inflight = func() map[string]int { return map[string]int{"w1|m": 3} }
	start := time.Now()
	if left, timedOut := o.awaitIdle(context.Background(), "w1", "m", opt); left != 3 || !timedOut || time.Since(start) < opt.DrainTimeout {
		t.Errorf("stuck: left %d, timed out %v after %s", left, timedOut, time.Since(start))
	}
	opt.Inflight = nil
	if _, timedOut := o.awaitIdle(context.Background(), "w1", "m", opt); !timedOut {
		t.Error("no counter must not read as idle")
	}
}

// One move at a time per model and per node.
func TestMoveGuard(t *testing.T) {
	var g moveGuard
	release, err := g.claim("m", "w1", "w2")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range [][3]string{{"m", "w3", "w4"}, {"other", "w2", "w5"}, {"other", "w5", "w1"}} {
		if _, err := g.claim(c[0], c[1], c[2]); !errors.Is(err, ErrUnplaceable) || !strings.Contains(err.Error(), "already running") {
			t.Errorf("%v while m moves w1 → w2: %v", c, err)
		}
	}
	if other, err := g.claim("other", "w3", "w4"); err != nil {
		t.Errorf("an unrelated move: %v", err)
	} else {
		other()
	}
	release()
	if again, err := g.claim("m", "w1", "w2"); err != nil {
		t.Errorf("after the first move ended: %v", err)
	} else {
		again()
	}
}
