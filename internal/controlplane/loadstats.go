package controlplane

// loadStats is the leader's cheap, in-memory picture of gateway load for an
// external autoscaler: per-second rings of inference-request starts and 5xx
// responses over the last 60 s, a live in-flight gauge, and the last-request
// timestamp. GET /loadz exposes it unauthenticated next to /healthz and
// /readyz — aggregate load numbers only, no request content, probe-grade
// trust. Only POSTs are counted: inference is always a POST, and GET
// /v1/models polling must not read as demand.

import (
	"context"
	"net/http"

	"github.com/opod-io/opod-sdk/nodeapi"

	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/router"

	"github.com/go-chi/chi/v5/middleware"
)

type loadStats struct {
	mu       sync.Mutex
	reqRing  [60]int64 // requests started, bucketed by absolute second % 60
	errRing  [60]int64 // 5xx responses
	reqSec   [60]int64 // absolute second each reqRing bucket counts for
	errSec   [60]int64
	inFlight int64 // atomic
	lastReq  int64 // atomic, unix seconds
}

func (l *loadStats) bump(ring *[60]int64, stamps *[60]int64, now time.Time) {
	sec := now.Unix()
	i := sec % 60
	l.mu.Lock()
	if stamps[i] != sec {
		stamps[i] = sec
		ring[i] = 0
	}
	ring[i]++
	l.mu.Unlock()
}

func (l *loadStats) sum(ring *[60]int64, stamps *[60]int64, now time.Time) int64 {
	cutoff := now.Unix() - 60
	var total int64
	l.mu.Lock()
	for i := range ring {
		if stamps[i] > cutoff {
			total += ring[i]
		}
	}
	l.mu.Unlock()
	return total
}

// ProbeHeader marks a request as a health probe rather than demand: served
// exactly like any other, counted like none of them.
//
// /loadz exists to answer "how busy is this leader?", and an autoscaler acts on
// the answer. A caller that asks the endpoint to prove it can serve — the
// control plane's first-token proof, a synthetic canary, an uptime check — is
// not a customer waiting for capacity, and counting it as one closes a loop the
// product cannot afford: on the design-partner cell (2026-09-20) a gang parked
// at floor 0 was woken 40 seconds later by the proof probe that the parking
// itself had triggered, reloading a model on two GPUs for nobody. A probe also
// leaves the idle clock alone, or nothing with a floor of 0 ever gets to idle.
//
// The leader cannot infer this: a probe is a well-formed request from a trusted
// caller. So the caller says so, and only a caller that already holds a key can
// (the header is read after auth).
const ProbeHeader = "X-Opod-Probe"

// isProbe reports whether r asked not to be counted as demand.
func isProbe(r *http.Request) bool {
	switch r.Header.Get(ProbeHeader) {
	case "", "0", "false":
		return false
	}
	return true
}

// trackLoad wraps the /v1 gateway routes.
func (s *Server) trackLoad(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || isProbe(r) {
			next.ServeHTTP(w, r)
			return
		}
		now := time.Now()
		atomic.StoreInt64(&s.load.lastReq, now.Unix())
		atomic.AddInt64(&s.load.inFlight, 1)
		defer atomic.AddInt64(&s.load.inFlight, -1)
		s.load.bump(&s.load.reqRing, &s.load.reqSec, now)
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if ww.Status() >= 500 {
			s.load.bump(&s.load.errRing, &s.load.errSec, time.Now())
		}
	})
}

// nodeLoadSample is a worker's last engine load, stamped when it arrived.
type nodeLoadSample struct {
	engines.EngineLoad
	at time.Time
}

// nodeEngineSample is a worker's last word on its engine process, stamped when
// it arrived (feature "engine_liveness"). Kept in memory beside the load
// samples: it describes a process that is running right now, and a leader that
// restarts learns it again within a heartbeat.
type nodeEngineSample struct {
	nodeapi.EngineState
	at time.Time
}

// EnginesUnhealthy counts the workers whose engine is NOT serving — crash
// looping, stopped, or stuck starting for longer than a model takes to load.
// Those workers hold their cards and heartbeat like any other, so anything
// counting workers counts them as capacity; they are not.
func (s *Server) EnginesUnhealthy() (unhealthy int, worst string) {
	s.nodeEngine.Range(func(_, v any) bool {
		e := v.(nodeEngineSample)
		if time.Since(e.at) > loadSampleMaxAge {
			return true // stale: the worker stopped saying, which liveness handles
		}
		switch e.State {
		case nodeapi.EngineCrashLooping, nodeapi.EngineStopped:
			unhealthy++
			if worst == "" || e.State == nodeapi.EngineCrashLooping {
				worst = e.State
				if e.Detail != "" {
					worst += ": " + e.Detail
				}
			}
		}
		return true
	})
	return unhealthy, worst
}

// loadSampleMaxAge: a worker's sample older than this is not "reporting" —
// a stuck engine must not hold a stale pressure number on /loadz.
const loadSampleMaxAge = 30 * time.Second

// loadz answers "how busy is this leader right now?" for the autoscaler.
func (s *Server) loadz(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	rev, planModel := s.plan.get()
	out := adminapi.Load{
		PlanRevision:    rev,
		PlanModel:       planModel,
		InFlight:        atomic.LoadInt64(&s.load.inFlight),
		RPM1m:           s.load.sum(&s.load.reqRing, &s.load.reqSec, now),
		Unavailable1m:   s.load.sum(&s.load.errRing, &s.load.errSec, now),
		LastRequestUnix: atomic.LoadInt64(&s.load.lastReq),
		TS:              now.Unix(),
	}
	s.aggregateWorkerLoad(r.Context(), &out, now)
	// How much of the worker count above is actually serving (feature
	// "engine_liveness"): a worker whose engine crash-loops heartbeats like any
	// other and holds its card, and a scaler that believes the count scales out
	// too late or not at all.
	out.EnginesUnhealthy, out.EngineIssue = s.EnginesUnhealthy()
	writeJSON(w, http.StatusOK, out)
}

// aggregateWorkerLoad folds the serving workers' engine samples into the
// leader's /loadz (build item 14): max KV use (one full cache is pressure),
// summed queue and tokens/s, mean prefix hits.
//
// `workers` is capacity a reader can count on: workers that can take a NEW
// request for the plan's model right now — the node takes new work
// (store.Node.TakesNewWork: not drained, heartbeating) and either holds a
// routable placement of it (any model when no plan names one) or a part of a
// gang of it that can serve. A draining worker, a lost one, one whose engine
// sleeps or is still loading is not counted, and its pressure is not reported:
// `reporting` is the counted workers with a fresh sample.
//
// The gang half is not a refinement: a SHARDED endpoint has no placement rows
// at all, so without it every number here stayed 0 while the endpoint answered
// requests, and an autoscaler reading kv_used_pct or queue_depth for a gang was
// reading a constant (found on the design-partner cell, 2026-09-20).
func (s *Server) aggregateWorkerLoad(ctx context.Context, out *adminapi.Load, now time.Time) {
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return
	}
	gangNodes := map[string]bool{}
	if shards, serr := s.store.Shards().List(ctx); serr == nil && len(shards) > 0 {
		gangNodes = servableGangNodes(shards, s.routableNodes(ctx), out.PlanModel)
	}
	maxAge := s.heartbeatMaxAge()
	var prefixSum float64
	for _, n := range nodes {
		if n.ID == "local" || !n.TakesNewWork(maxAge, now) {
			continue
		}
		if !gangNodes[n.ID] && !s.servesNow(ctx, n.ID, out.PlanModel) {
			continue
		}
		out.Workers++
		v, ok := s.nodeLoad.Load(n.ID)
		if !ok {
			continue
		}
		ld := v.(nodeLoadSample)
		if now.Sub(ld.at) > loadSampleMaxAge {
			continue
		}
		out.Reporting++
		if ld.KVUsedPct > out.KVUsedPct {
			out.KVUsedPct = ld.KVUsedPct
		}
		out.QueueDepth += ld.QueueDepth
		out.TokensPerSec += ld.TokensPerSec
		prefixSum += ld.PrefixHitPct
	}
	if out.Reporting > 0 {
		out.PrefixHitPct = prefixSum / float64(out.Reporting)
	}
}

// servesNow reports whether the node holds a routable placement of model
// (of any model when model is ""): the row the router would route to.
func (s *Server) servesNow(ctx context.Context, nodeID, model string) bool {
	ps, err := s.store.Placements().GetByNode(ctx, nodeID)
	if err != nil {
		return false
	}
	for _, p := range ps {
		if (p.Status == "" || p.Status == "ready") && (model == "" || p.ModelID == model) {
			return true
		}
	}
	return false
}

// loadSignal is the router's LoadSource: a worker's last engine sample when
// it is fresh (loadSampleMaxAge), else nothing — a stuck engine must not
// keep a stale pressure number in the pick order any more than on /loadz.
func (s *Server) loadSignal(nodeID string) (router.LoadSignal, bool) {
	v, ok := s.nodeLoad.Load(nodeID)
	if !ok {
		return router.LoadSignal{}, false
	}
	ld := v.(nodeLoadSample)
	if time.Since(ld.at) > loadSampleMaxAge {
		return router.LoadSignal{}, false
	}
	return router.LoadSignal{KVUsedPct: ld.KVUsedPct, QueueDepth: ld.QueueDepth}, true
}
