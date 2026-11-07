package leader

// The usage push (feature usage_push, T32.34). The leader keeps no usage
// table: every finished request — its own and those its front doors push here
// (gatewayspend.go) — is a record in a bounded in-memory queue, and an exporter
// sends the oldest to the OTLP/HTTP logs URL in OPOD_USAGE_OTLP_ENDPOINT,
// freeing them only on a 2xx (internal/usage). The leader does not know what
// is behind the URL: a collector, or a manager that renders the URL and a
// credential into the leader's environment.

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/opod-io/opod-core/internal/usage"
)

// usageQueueBytes reads OPOD_USAGE_QUEUE_MB. A value that is not a positive
// whole number is reported and the default used: a typo must not cost the
// usage records an outage would otherwise keep.
func usageQueueBytes(mb string, log *slog.Logger) int {
	if mb == "" {
		return usage.DefaultQueueMB << 20
	}
	n, err := strconv.Atoi(mb)
	if err != nil || n <= 0 {
		log.Warn("OPOD_USAGE_QUEUE_MB is not a positive whole number of MiB; using the default", "value", mb, "default_mb", usage.DefaultQueueMB)
		return usage.DefaultQueueMB << 20
	}
	return n << 20
}

// startUsagePush runs the exporter when a URL is set. Without one the queue
// still bounds memory, and the leader says so once at start and on /metrics
// (opod_usage_queue_records grows to the cap, then opod_usage_dropped_total).
func (s *Server) startUsagePush(ctx context.Context) {
	if s.isGateway() {
		return // a door's records ride its push to the leader
	}
	u := s.cfg.Env.UsageOTLPEndpoint
	if u == "" {
		s.log.Info("usage push off: records are held in memory and the oldest dropped at the cap (set OPOD_USAGE_OTLP_ENDPOINT)",
			"cap_mb", s.usage.Stats().Cap>>20)
		return
	}
	exp, err := usage.NewExporter(s.usage, u, s.cfg.Env.UsageOTLPHeaders)
	if err != nil {
		// A bad URL or header is configuration, not an outage: say it loudly,
		// keep serving, keep the records bounded.
		s.log.Error("usage push not started", "err", err)
		return
	}
	s.log.Info("usage push on", "url", u, "cap_mb", s.usage.Stats().Cap>>20)
	go exp.Run(ctx)
}

// UsageLastUsed is the newest request per model since this process started:
// the lifecycle manager's least-recently-used order (it read the usage
// table's MAX(ts) before the table left).
func (s *Server) UsageLastUsed() map[string]time.Time { return s.usage.LastUsed() }
