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
	"github.com/opod-io/opod/internal/store"

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
// looping or stopped. Those workers hold their cards and heartbeat like any
// other, so anything counting workers counts them as capacity; they are not.
//
// Since T11.2 a worker that reported `stopped` is already out of the `workers`
// count and out of rotation — the state is terminal and it is what a worker
// sends on its way out. It is still counted HERE, because the fact this number
// exists to carry is a card held by a process that is not serving, and that is
// still true. A crash-looping one is counted in both: it may be up again by the
// time the next request lands, so it keeps its place in rotation.
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

// engineGoneWhy is the reason a worker cannot take new work because of what it
// SAID about its own engine, or "" when it has said no such thing (T11.2,
// ADR-065).
//
// Only `stopped` counts, and the distinction is the whole decision:
//
//   - `stopped` is terminal by its own definition — "exited and not restarting;
//     it will not come back without a change" — and it is what a worker sends on
//     its way out (agent.Goodbye). Acting on it closes the window every park and
//     every rolling update has, where a pod already killed its engine, keeps
//     heartbeating through the grace period, and the leader routes a request
//     into a dead process and answers 502: the one error class
//     `capacity.go` says a client cannot act on.
//   - `crash-looping` does NOT count. It is a report about capacity that keeps
//     disappearing, not a statement that it is gone; the process may be up again
//     by the time the next request lands, and taking it out of rotation on a
//     report would flap an endpoint between serving and 503. It stays what it
//     was: a number on /loadz and a word in an alert.
//
// A stale sample counts for nothing — the heartbeat rule owns silence (C6).
func (s *Server) engineGoneWhy(nodeID string) string {
	v, ok := s.nodeEngine.Load(nodeID)
	if !ok {
		return ""
	}
	e := v.(nodeEngineSample)
	if time.Since(e.at) > loadSampleMaxAge || e.State != nodeapi.EngineStopped {
		return ""
	}
	if e.Detail != "" {
		return "engine stopped: " + e.Detail
	}
	return "engine stopped"
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
	var prefixSamples int
	for _, n := range nodes {
		if n.ID == "local" || !n.TakesNewWork(maxAge, now) {
			continue
		}
		// A worker that said its engine is gone is not capacity a reader can
		// count on, and the router will not send it a request either (T11.2).
		// The card it still holds is reported below, as EnginesUnhealthy.
		if s.engineGoneWhy(n.ID) != "" {
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
		prefixSamples++
		if ld.KVUsedPct > out.KVUsedPct {
			out.KVUsedPct = ld.KVUsedPct
		}
		out.QueueDepth += ld.QueueDepth
		out.TokensPerSec += ld.TokensPerSec
		prefixSum += ld.PrefixHitPct
	}
	// A gang reports through its COORDINATOR, not its parts. The parts are
	// rpc-servers: they hold weights and do matrix multiplication, they run no
	// engine and have no queue, no KV cache and no notion of a request — so
	// they send no engine sample and never will. The process that has all
	// three is the coordinator, and it is not a registered worker's engine
	// (it is the leader's own supervised process when the head is local, and a
	// process the head worker was told to start when it is not), so nothing
	// above sees it.
	//
	// Until this, `kv_used_pct`, `queue_depth` and `tokens_per_s` were 0 for a
	// sharded endpoint however loaded it was — two of the five triggers the
	// control plane renders for a gang could not fire, and the endpoint could
	// only ever scale on in-flight, RPM and unavailability (measured on the
	// design-partner cell, 2026-09-20).
	for _, c := range s.gangCoordinators(ctx, out.PlanModel) {
		ld, ok := s.gangSample(ctx, c)
		if !ok {
			continue
		}
		out.Reporting++
		prefixSamples++
		if ld.KVUsedPct > out.KVUsedPct {
			out.KVUsedPct = ld.KVUsedPct
		}
		out.QueueDepth += ld.QueueDepth
		out.TokensPerSec += ld.TokensPerSec
		prefixSum += ld.PrefixHitPct
	}
	// The mean is over the SAMPLES, not the workers: one gang's sample speaks
	// for every part of it, so dividing by the worker count would read a gang's
	// prefix-cache hit rate as a fraction of itself.
	if prefixSamples > 0 {
		out.PrefixHitPct = prefixSum / float64(prefixSamples)
	}
}

// gangCoordinators is the coordinator row of every gang of model that can
// serve right now. Empty for an endpoint with no gangs, which is the common
// case and costs one store read.
func (s *Server) gangCoordinators(ctx context.Context, model string) []store.Shard {
	shards, err := s.store.Shards().List(ctx)
	if err != nil || len(shards) == 0 {
		return nil
	}
	return servableGangCoordinators(shards, s.routableNodes(ctx), model)
}

// gangCoordEngine is a coordinator's driver with the address it was built for:
// a gang that re-formed elsewhere gets a new one rather than a client pointed
// at a pod that is gone.
type gangCoordEngine struct {
	addr string
	eng  engines.Engine
}

// gangEngine returns the driver for this coordinator, building it once. The
// driver name comes from the row (router.CoordinatorEngine): vLLM and SGLang
// coordinators expose their own metric names, llama.cpp another set.
func (s *Server) gangEngine(coord store.Shard) (engines.Engine, bool) {
	if v, ok := s.gangEng.Load(coord.ID); ok {
		if ge := v.(gangCoordEngine); ge.addr == coord.Address {
			return ge.eng, true
		}
	}
	eng := engines.MustNew(router.CoordinatorEngine(coord), "http://"+coord.Address, "")
	s.gangEng.Store(coord.ID, gangCoordEngine{addr: coord.Address, eng: eng})
	return eng, true
}

// gangSampleTTL bounds how often a coordinator is scraped, whatever the rate
// /loadz is polled at. It mirrors the worker heartbeat cadence (5 s), which is
// how often a worker's own engine sample is refreshed, and sits well inside
// loadSampleMaxAge so a cached sample is never a stale one. /loadz is
// unauthenticated and probe-grade: without this, its poll rate would be the
// coordinator's scrape rate.
const gangSampleTTL = 5 * time.Second

// gangSample scrapes one gang coordinator for its pressure, through the same
// engine driver the router dials it with (the row records the driver name;
// llama.cpp when it records none). Cached for gangSampleTTL.
//
// Best-effort by design, exactly like the worker path: a coordinator that is
// slow, gone or built without metrics yields no sample, the gang stays counted
// in `workers` — it IS serving — and `reporting` says its pressure is unknown.
func (s *Server) gangSample(ctx context.Context, coord store.Shard) (engines.EngineLoad, bool) {
	if coord.Address == "" {
		return engines.EngineLoad{}, false
	}
	if v, ok := s.gangLoad.Load(coord.ID); ok {
		if c := v.(nodeLoadSample); time.Since(c.at) < gangSampleTTL {
			return c.EngineLoad, true
		}
	}
	// The driver is kept, not rebuilt. `tokens_per_s` is a RATE the driver
	// holds between samples (genRate in the vLLM and llama.cpp drivers), so a
	// fresh driver per scrape has no previous reading and reports 0 for ever —
	// measured on the design-partner cell (2026-09-20): a gang under 16
	// concurrent requests showed kv_used_pct rising and tokens_per_s flat 0.
	// The router keeps its coordinator clients for the same reason.
	eng, ok := s.gangEngine(coord)
	if !ok {
		return engines.EngineLoad{}, false
	}
	lr, ok := eng.(engines.LoadReporter)
	if !ok {
		return engines.EngineLoad{}, false
	}
	sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	ld, err := lr.Load(sctx)
	if err != nil {
		return engines.EngineLoad{}, false
	}
	s.gangLoad.Store(coord.ID, nodeLoadSample{EngineLoad: ld, at: time.Now()})
	return ld, true
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
