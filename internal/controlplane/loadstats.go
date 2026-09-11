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

	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"
	"github.com/opod-io/opod/internal/engines"

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

// trackLoad wraps the /v1 gateway routes.
func (s *Server) trackLoad(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
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
	writeJSON(w, http.StatusOK, out)
}

// aggregateWorkerLoad folds the live workers' engine samples into the
// leader's /loadz (build item 14): max KV use (one full cache is pressure),
// summed queue and tokens/s, mean prefix hits. Only alive workers count
// (liveness.go), only fresh samples report.
func (s *Server) aggregateWorkerLoad(ctx context.Context, out *adminapi.Load, now time.Time) {
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return
	}
	maxAge := s.heartbeatMaxAge()
	var prefixSum float64
	for _, n := range nodes {
		if n.ID == "local" || !nodeAlive(n, maxAge, now) {
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
