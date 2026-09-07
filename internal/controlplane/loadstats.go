package controlplane

// loadStats is the leader's cheap, in-memory picture of gateway load for an
// external autoscaler: per-second rings of inference-request starts and 5xx
// responses over the last 60 s, a live in-flight gauge, and the last-request
// timestamp. GET /loadz exposes it unauthenticated next to /healthz and
// /readyz — aggregate load numbers only, no request content, probe-grade
// trust. Only POSTs are counted: inference is always a POST, and GET
// /v1/models polling must not read as demand.

import (
	"github.com/opod-io/opod/pkg/adminapi"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

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

// loadz answers "how busy is this leader right now?" for the autoscaler.
func (s *Server) loadz(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	rev, planModel := s.plan.get()
	writeJSON(w, http.StatusOK, adminapi.Load{
		PlanRevision:    rev,
		PlanModel:       planModel,
		InFlight:        atomic.LoadInt64(&s.load.inFlight),
		RPM1m:           s.load.sum(&s.load.reqRing, &s.load.reqSec, now),
		Unavailable1m:   s.load.sum(&s.load.errRing, &s.load.errSec, now),
		LastRequestUnix: atomic.LoadInt64(&s.load.lastReq),
		TS:              now.Unix(),
	})
}
