package leader

// vLLM-named metric aliases on /metrics (PLAN T17.8, ADR-083), so a leader can
// be a member of a Gateway API inference pool whose endpoint picker already
// reads vLLM's names, with no mapping written on the customer's side.
//
// The picker is llm-d's (the Gateway API Inference Extension's endpoint picker
// moved there). Its default engine mapping for "vllm" was read from source:
//
//	github.com/llm-d/llm-d-inference-scheduler @ 6ca663eefe2b (2026-10-06; Go module path
//	github.com/llm-d/llm-d-router), pkg/epp/framework/plugins/datalayer/extractor/metrics/factories.go:
//	  QueuedRequestsSpec:  "vllm:num_requests_waiting"
//	  RunningRequestsSpec: "vllm:num_requests_running"
//	  KVUsageSpec:         "vllm:kv_cache_usage_perc"
//	  LoRASpec:            "vllm:lora_requests_info"   (optional; skipped silently when absent)
//	  CacheInfoSpec:       "vllm:cache_config_info"    (block size, for prefix scoring)
//
// The KV figure is a FRACTION 0–1 despite its name: the KV-utilisation scorer
// scores an endpoint 1 − KVCacheUsagePercent
// (pkg/epp/framework/plugins/scheduling/scorer/kvcacheutilization/kvcache_utilization.go).
//
// What a leader publishes, and why each aggregate is the one it is — the picker
// compares this leader as ONE pod against other pods, and the leader itself
// picks the worker inside:
//
//   - vllm:num_requests_waiting  the SUM of the queue depths the leader's workers
//     (and gang coordinators) report: every one is a request this endpoint
//     has not started.
//   - vllm:num_requests_running  the leader's own live in-flight count minus that
//     queue, floored at 0. vLLM splits the requests it holds into running and
//     waiting; in-flight is the whole, so the remainder is the running part.
//   - vllm:kv_cache_usage_perc   the MEAN KV-cache use over the reporting
//     samples, as a fraction. Not the maximum /loadz carries: one full worker
//     beside three idle ones is an endpoint with room, and the leader routes
//     around the full one.
//
// The two optional families are not published. The LoRA info is skipped by the
// picker when absent; the cache-config info is a fact about one engine's block
// size that an endpoint of several workers does not have, and publishing a
// made-up one would steer prefix scoring wrongly (its absence counts one
// extract error per poll on the picker's side, nothing more).
//
// The aliases sit BESIDE the opod_* names, never instead (ADR-083): nothing
// that reads our own metrics moves. Off the request path; ADR-001 is untouched.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// VLLMAliasNames is the alias set a test holds: removing one breaks an
// inference pool that routes to this leader, so it fails the build.
var VLLMAliasNames = []string{
	"vllm:num_requests_waiting",
	"vllm:num_requests_running",
	"vllm:kv_cache_usage_perc",
}

// aliasSampleTTL bounds how often the worker aggregate behind the aliases is
// recomputed. The picker polls every pod at tens of hertz, and the aggregate is
// store reads; the worker figures it is made of only change on a heartbeat
// (5 s). The in-flight count is read live on every scrape.
const aliasSampleTTL = time.Second

// vllmAliasCollector computes the aliases at scrape time from what the leader
// already knows: its in-flight gauge and its workers' last engine samples.
type vllmAliasCollector struct {
	s *Server

	waiting, running, kv *prometheus.Desc

	mu     sync.Mutex
	at     time.Time
	model  string
	queue  float64
	kvFrac float64
}

func newVLLMAliasCollector(s *Server) *vllmAliasCollector {
	labels := []string{"model_name"} // vLLM's own label, so the series look like an engine's
	return &vllmAliasCollector{
		s: s,
		waiting: prometheus.NewDesc("vllm:num_requests_waiting",
			"opod alias (ADR-083): requests this endpoint's engines report queued, summed over its workers.", labels, nil),
		running: prometheus.NewDesc("vllm:num_requests_running",
			"opod alias (ADR-083): requests the leader has in flight that its engines do not report queued.", labels, nil),
		kv: prometheus.NewDesc("vllm:kv_cache_usage_perc",
			"opod alias (ADR-083): mean KV-cache use over the reporting workers, as a fraction 0-1.", labels, nil),
	}
}

func (c *vllmAliasCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.waiting
	ch <- c.running
	ch <- c.kv
}

func (c *vllmAliasCollector) Collect(ch chan<- prometheus.Metric) {
	model, queue, kvFrac := c.sample()
	running := float64(atomic.LoadInt64(&c.s.load.inFlight)) - queue
	if running < 0 {
		running = 0
	}
	ch <- prometheus.MustNewConstMetric(c.waiting, prometheus.GaugeValue, queue, model)
	ch <- prometheus.MustNewConstMetric(c.running, prometheus.GaugeValue, running, model)
	ch <- prometheus.MustNewConstMetric(c.kv, prometheus.GaugeValue, kvFrac, model)
}

// sample is the worker aggregate, recomputed at most once per aliasSampleTTL.
func (c *vllmAliasCollector) sample() (model string, queue, kvFrac float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if !c.at.IsZero() && now.Sub(c.at) < aliasSampleTTL {
		return c.model, c.queue, c.kvFrac
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ld := c.s.workerLoad(ctx, now)
	c.at, c.model, c.queue, c.kvFrac = now, ld.PlanModel, float64(ld.QueueDepth), ld.kvMeanFrac
	return c.model, c.queue, c.kvFrac
}

// metrics returns the one /metrics handler both listeners serve.
func (s *Server) metrics() http.Handler {
	s.metricsOnce.Do(func() { s.metricsH = s.metricsHandler() })
	return s.metricsH
}

// metricsHandler serves /metrics on both listeners: the process-wide registry
// (every opod_* name) and this leader's own registry carrying the aliases. The
// aliases are per Server rather than global so a process — or a test — with
// several leaders never registers one collector twice.
func (s *Server) metricsHandler() http.Handler {
	reg := prometheus.NewRegistry()
	reg.MustRegister(newVLLMAliasCollector(s))
	return promhttp.InstrumentMetricHandler(prometheus.DefaultRegisterer,
		promhttp.HandlerFor(prometheus.Gatherers{prometheus.DefaultGatherer, reg}, promhttp.HandlerOpts{}))
}
