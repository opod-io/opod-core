package leader

// The request path's wake (feature "request_wake").
//
// A sleeping engine keeps its process and its weights in host memory so that
// waking costs less than a caller notices — vLLM's /wake_up after a level-1
// sleep is sub-second. Until this file the leader answered such a request
// 503 "waking … retry shortly" and left the resume to whoever manages the
// endpoint, on that manager's own clock: measured on a cluster, the caller
// was refused and the engine woke ten seconds later. That is the cost of a
// PARKED endpoint (a pod has to start, ADR-065), charged for a sleep.
//
// So the leader resumes the worker itself, holds the request while it does,
// and serves it. The decision is on the request path and therefore the
// leader's (a manager pushes policy, never sits on the path); a manager still
// decides WHEN an engine sleeps.
//
// Bounds. The hold is requestWakeBudget: a resume that does not answer inside
// it falls back to the 503 + Retry-After every other "not right now" gets.
// One resume per worker at a time — a burst against a sleeping endpoint waits
// on the same call rather than sending one each. A worker whose engine has no
// sleep mode answers 501 and is left alone.

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// requestWakeBudget is how long a request is held for a sleeping engine to
// resume before it is answered 503.
var requestWakeBudget = 3 * time.Second // a var so a test can shorten it

// wakeFlight is one in-progress resume, shared by every request that arrives
// while it runs.
type wakeFlight struct {
	done chan struct{}
	ok   bool
}

var wakeFlights sync.Map // node id → *wakeFlight

// wakeForRequest resumes a sleeping worker that holds model and reports
// whether the model can be served now. False when nothing is asleep for it,
// the resume failed, or it did not answer inside the budget.
func (s *Server) wakeForRequest(ctx context.Context, model string) bool {
	n := s.sleepingHolder(ctx, model)
	if n == nil {
		return false
	}
	f, running := wakeFlights.LoadOrStore(n.ID, &wakeFlight{done: make(chan struct{})})
	flight := f.(*wakeFlight)
	if !running {
		go func() {
			defer func() { wakeFlights.Delete(n.ID); close(flight.done) }()
			flight.ok = s.resumeNode(n, model)
		}()
	}
	t := time.NewTimer(requestWakeBudget)
	defer t.Stop()
	select {
	case <-flight.done:
		return flight.ok && s.modelServable(ctx, model)
	case <-t.C:
		return false // the resume goes on; this caller gets 503 + Retry-After
	case <-ctx.Done():
		return false
	}
}

// sleepingHolder is a live worker that holds model asleep, or nil.
func (s *Server) sleepingHolder(ctx context.Context, model string) *store.Node {
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return nil
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for i := range nodes {
		n := nodes[i]
		if n.ID == "local" || n.Address == "" || !n.TakesNewWork(maxAge, now) {
			continue
		}
		ps, err := s.store.Placements().GetByNode(ctx, n.ID)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if p.ModelID == model && p.Status == PlacementSleeping {
				return &n
			}
		}
	}
	return nil
}

// resumeNode wakes one worker's engine and, on the worker's own 200, makes
// its sleeping placements routable at once — the next heartbeat would say the
// same five seconds later, which is five seconds the caller is waiting.
func (s *Server) resumeNode(n *store.Node, model string) bool {
	// Its own context: the resume must finish even when the request that
	// asked for it gave up, or the next caller starts it again from nothing.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	code, _, err := s.callWorkerSleep(ctx, n, "resume")
	if err != nil || code != http.StatusOK {
		s.log.Warn("request wake: resume refused", "node", n.ID, "status", code, "err", err)
		return false
	}
	ps, err := s.store.Placements().GetByNode(ctx, n.ID)
	if err != nil {
		return false
	}
	for _, p := range ps {
		if p.Status == PlacementSleeping {
			if err := s.store.Placements().SetStatus(ctx, n.ID, p.ModelID, "ready"); err != nil {
				s.log.Warn("request wake: placement not marked ready", "node", n.ID, "model", p.ModelID, "err", err)
				return false
			}
		}
	}
	s.router.InvalidateModel("")
	s.record("worker.resume", n.ID, map[string]any{"node": n.ID, "by": "request", "model": model, "ms": time.Since(started).Milliseconds()})
	return true
}
