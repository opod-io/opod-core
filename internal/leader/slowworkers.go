package leader

// Workers the router ranks down for latency (feature "worker_slow", ADR-122
// §4, PLAN T28.12): named on /loadz and recorded on the event stream, so a
// manager can alert. The manager never decides the route — the router does,
// from what it serves (router/slow.go).

import (
	"github.com/opod-io/opod-core/internal/router"
	"github.com/opod-io/opod-core/pkg/adminapi"
)

// Event types for a worker turning slow and being cleared. Subject is the
// router key: the worker's node id, or a gang's "shard:<model>:<gang>".
const (
	eventWorkerSlow        = "worker.slow"
	eventWorkerSlowCleared = "worker.slow_cleared"
)

// recordSlow is the router's slow hook: one event per change of state.
func (s *Server) recordSlow(c router.SlowChange) {
	typ := eventWorkerSlowCleared
	if c.Slow {
		typ = eventWorkerSlow
	}
	s.record(typ, c.Key, map[string]any{"model": c.Model, "factor": c.Factor, "metric": c.Metric})
}

// slowWorkers is /loadz's slow_workers: nil when the router ranks none down.
func (s *Server) slowWorkers() []adminapi.SlowWorker {
	if s.router == nil {
		return nil
	}
	ws := s.router.SlowWorkers()
	if len(ws) == 0 {
		return nil
	}
	out := make([]adminapi.SlowWorker, len(ws))
	for i, w := range ws {
		out[i] = adminapi.SlowWorker{Worker: w.Key, Model: w.Model, Factor: w.Factor, Metric: w.Metric, Samples: w.Samples}
	}
	return out
}
