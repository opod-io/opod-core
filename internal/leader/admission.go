package leader

// The admission hold (ADR-082, feature "admission_hold").
//
// A request that finds nothing able to take it has always been answered at once
// with 503 + Retry-After (dispatch.go). That is honest, and under a burst it
// makes every client retry at the same moment — the herd a hold exists to
// prevent. So the policy snapshot may give the leader a budget:
// admission.holdMs. A request that finds no capacity waits up to that long for
// some, is served the moment there is, and is shed with exactly the old 503
// when the budget runs out.
//
// What "no capacity" means here is precisely what the old 503 meant:
// Server.unavailable — no worker that takes new work holds the model, no gang
// of it can serve, the endpoint is waking. The leader does not refuse for load
// (the router's saturation rule only reorders workers), so there is no other
// "full" to hold for; an engine's own 429/503 is an answer to a request already
// dispatched, and the router's retries own that.
//
// Rules, each the ADR's:
//   - holdMs 0 (or absent) skips all of this: no counter, no wait, the old 503.
//   - First come, first served. Nothing is ranked; when capacity returns every
//     held request re-checks and goes, exactly as an unheld one would.
//   - maxHeld caps how many wait at once; past it a request is shed at once,
//     so a burst cannot pile up connections without bound. 0 = defaultMaxHeld.
//   - The budget is the request's, from the moment it reached dispatch: a wake
//     (wakeonrequest.go) spends from it rather than adding to it, so no caller
//     waits longer than max(holdMs, the wake budget).
//   - A client that goes away releases its slot at once.
//
// Waiting is on the request's own goroutine and on a signal, never a poll: the
// leader's own writes of who-can-serve — a heartbeat, a registration, an
// undrain, a resume, a model or gang coming up — call capacityChanged, and
// every held request re-checks. A source of capacity that does not call it
// costs a held request its budget and the old 503, never a wrong answer.

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/metrics"
)

// PolicyAdmission is the shared wire type (opod-sdk/adminapi).
type PolicyAdmission = adminapi.PolicyAdmission

// defaultMaxHeld is the cap when the policy sets a hold and no maxHeld. A held
// request costs an idle goroutine, its open connection and a body the gateway
// already capped; 64 of those is nothing to the leader, and is several times
// the concurrency one engine serves — enough to absorb a burst, small enough
// that a stampede is shed instead of queued.
const defaultMaxHeld = 64

type admissionState struct {
	holdMs  atomic.Int64
	maxHeld atomic.Int64
	held    atomic.Int64
}

// set installs the policy's admission half. Negative values are 0.
func (a *admissionState) set(p PolicyAdmission) {
	a.holdMs.Store(int64(max(p.HoldMs, 0)))
	a.maxHeld.Store(int64(max(p.MaxHeld, 0)))
}

// acquire takes a hold slot, or reports the cap is reached. The cap is read
// per call, so a policy change applies to the next request.
func (a *admissionState) acquire() bool {
	limit := a.maxHeld.Load()
	if limit <= 0 {
		limit = defaultMaxHeld
	}
	for {
		n := a.held.Load()
		if n >= limit {
			return false
		}
		if a.held.CompareAndSwap(n, n+1) {
			metrics.SetAdmissionHeld(n + 1)
			return true
		}
	}
}

func (a *admissionState) release() { metrics.SetAdmissionHeld(a.held.Add(-1)) }

// capacitySignal wakes every held request when who-can-serve may have changed.
// A channel closed on notify and replaced on the next wait: a broadcast with
// no per-waiter state, and nothing allocated while nobody waits.
type capacitySignal struct {
	mu sync.Mutex
	ch chan struct{}
}

// wait returns a channel closed by the next notify. Take it BEFORE checking
// the condition, so a change between the check and the wait is not missed.
func (c *capacitySignal) wait() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch == nil {
		c.ch = make(chan struct{})
	}
	return c.ch
}

func (c *capacitySignal) notify() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch != nil {
		close(c.ch)
		c.ch = nil
	}
}

// capacityChanged tells held requests to look again. Cheap and safe to call
// on any write that may make a model servable; a call that changed nothing
// costs each held request one re-check.
func (s *Server) capacityChanged() { s.capacity.notify() }

// holdForCapacity waits, within the policy's budget counted from started, for
// model to become servable. It returns "" when it did — serve the request —
// or the reason to shed it with, the latest one seen.
func (s *Server) holdForCapacity(ctx context.Context, model, reason string, started time.Time) string {
	budget := time.Duration(s.admission.holdMs.Load()) * time.Millisecond
	if budget <= 0 {
		return reason
	}
	left := budget - time.Since(started)
	if left <= 0 {
		metrics.ObserveAdmissionHold("shed_deadline")
		return reason
	}
	if !s.admission.acquire() {
		metrics.ObserveAdmissionHold("shed_cap")
		return reason
	}
	defer s.admission.release()
	deadline := time.NewTimer(left)
	defer deadline.Stop()
	for {
		changed := s.capacity.wait()
		why := s.unavailable(ctx, model)
		if why == "" {
			metrics.ObserveAdmissionHold("served")
			return ""
		}
		reason = why
		select {
		case <-changed:
		case <-deadline.C:
			metrics.ObserveAdmissionHold("shed_deadline")
			return reason
		case <-ctx.Done():
			metrics.ObserveAdmissionHold("cancelled")
			return reason
		}
	}
}
