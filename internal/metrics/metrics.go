// Package metrics declares all Prometheus instruments for Opod.
//
// Conventions:
//   - All metric names are prefixed with "opod_"
//   - Histograms use buckets in seconds (TTFT, request duration)
//   - Counters are accumulated per outcome label so error rate is computable
//
// The /metrics route is wired up in controlplane.routes.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_requests_total",
		Help: "Total number of inference requests by model, protocol, and outcome.",
	}, []string{"model", "protocol", "outcome"})

	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opod_request_duration_seconds",
		Help:    "End-to-end request duration in seconds.",
		Buckets: []float64{0.1, 0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120, 300},
	}, []string{"model", "protocol", "outcome"})

	tokensTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_request_tokens_total",
		Help: "Total tokens served by model and direction (prompt|completion).",
	}, []string{"model", "direction"})

	modelLoaded = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_model_loaded",
		Help: "1 if the model is currently loaded on this node, 0 otherwise.",
	}, []string{"model", "node"})

	nodeUp = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_node_up",
		Help: "1 if the node has heartbeated recently, 0 otherwise.",
	}, []string{"node", "hostname"})

	routerPicksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_router_picks_total",
		Help: "Router dispatch decisions by path (local|worker|shard|fallback-to-local) and outcome (ok|error|stale-heartbeat).",
	}, []string{"path", "outcome"})

	routerInflight = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_router_inflight",
		Help: "Current in-flight request count per node, as seen by the router.",
	}, []string{"node"})

	routerFallbackTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_router_fallback_total",
		Help: "Fallback chain activations by operation (chat|embed) and reason (primary-error|latency-reorder|cap-exhausted).",
	}, []string{"op", "reason"})

	routerAttemptDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opod_router_attempt_duration_seconds",
		Help:    "Per-attempt duration in seconds (chat = start-to-stream-done, embed = full response).",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60, 120, 300},
	}, []string{"model", "outcome"})

	routerCooldownsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "opod_router_cooldowns_active",
		Help: "Number of worker nodes currently in the placement-cooldown penalty box.",
	})

	routerStickyOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_router_sticky_hits_total",
		Help: "Per-(user_id, model) session stickiness outcomes (hit|miss|expired). 'hit' = the previously-pinned worker served this request; 'miss' = no fresh pin existed; 'expired' = pin existed but the TTL had passed.",
	}, []string{"outcome"})

	callbackSentTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_callback_sent_total",
		Help: "Observability callback delivery attempts per sink and outcome (ok | failed | dropped | exhausted | cancelled).",
	}, []string{"sink", "outcome"})

	callbackQueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_callback_queue_depth",
		Help: "Per-sink callback queue depth (events buffered but not yet sent).",
	}, []string{"sink"})
)

// ObserveCallback records a delivery attempt for a sink.
//
// outcome ∈ {"ok", "failed", "dropped", "exhausted", "cancelled"}.
func ObserveCallback(sink, outcome string) {
	callbackSentTotal.WithLabelValues(sink, outcome).Inc()
}

// SetCallbackQueueDepth updates the per-sink queue gauge after a send
// or enqueue.
func SetCallbackQueueDepth(sink string, n int) {
	callbackQueueDepth.WithLabelValues(sink).Set(float64(n))
}

var guardrailActionTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "opod_guardrail_action_total",
	Help: "Guardrail verdicts per guardrail name and action (allow|block|rewrite|flag).",
}, []string{"name", "action"})

// ObserveGuardrail records the verdict of one guardrail check.
func ObserveGuardrail(name, action string) {
	guardrailActionTotal.WithLabelValues(name, action).Inc()
}

var (
	cacheHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_cache_hits_total",
		Help: "Response cache hits per endpoint path.",
	}, []string{"path"})

	cacheMissesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_cache_misses_total",
		Help: "Response cache misses per endpoint path.",
	}, []string{"path"})
)

// ObserveCacheHit records a response-cache hit on the given endpoint.
func ObserveCacheHit(path string) { cacheHitsTotal.WithLabelValues(path).Inc() }

// ObserveCacheMiss records a response-cache miss on the given endpoint.
func ObserveCacheMiss(path string) { cacheMissesTotal.WithLabelValues(path).Inc() }

var routerHedgeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "opod_router_hedge_total",
	Help: "Per-replica outcomes for hedged requests (win|cancelled|error).",
}, []string{"outcome"})

// ObserveRouterHedge records one replica's outcome.
//
// outcome ∈ {"win","cancelled","error"}.
func ObserveRouterHedge(outcome string) {
	routerHedgeTotal.WithLabelValues(outcome).Inc()
}

// ObserveStickyOutcome bumps the per-outcome counter for sticky-session
// behavior. outcome ∈ {"hit", "miss", "expired"}.
func ObserveStickyOutcome(outcome string) {
	routerStickyOutcomes.WithLabelValues(outcome).Inc()
}

// SetRouterCooldownsActive sets the gauge for placements currently in
// the cooldown penalty box. Called from the Router whenever a node
// enters or exits cooldown.
func SetRouterCooldownsActive(n int) {
	routerCooldownsActive.Set(float64(n))
}

// ObserveRouterPick records a dispatch decision. Path is one of
// local | worker | shard | fallback-to-local; outcome is ok | error | stale-heartbeat.
func ObserveRouterPick(path, outcome string) {
	routerPicksTotal.WithLabelValues(path, outcome).Inc()
}

// SetRouterInflight sets the live inflight count for a node. Called from
// inc/dec hooks so the gauge mirrors the router's view exactly.
func SetRouterInflight(node string, n int) {
	routerInflight.WithLabelValues(node).Set(float64(n))
}

// ObserveRouterFallback records a fallback activation. op is chat | embed;
// reason is primary-error | latency-reorder | cap-exhausted.
func ObserveRouterFallback(op, reason string) {
	routerFallbackTotal.WithLabelValues(op, reason).Inc()
}

// ObserveRouterAttempt records per-attempt duration and outcome.
func ObserveRouterAttempt(model, outcome string, dur time.Duration) {
	routerAttemptDuration.WithLabelValues(model, outcome).Observe(dur.Seconds())
}

// ObserveRequest records the outcome of a single inference request.
func ObserveRequest(model, protocol, outcome string, dur time.Duration, promptTokens, completionTokens int) {
	requestsTotal.WithLabelValues(model, protocol, outcome).Inc()
	requestDuration.WithLabelValues(model, protocol, outcome).Observe(dur.Seconds())
	if promptTokens > 0 {
		tokensTotal.WithLabelValues(model, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		tokensTotal.WithLabelValues(model, "completion").Add(float64(completionTokens))
	}
}

// SetModelLoaded marks a model as loaded (1) or not (0) on a node.
func SetModelLoaded(model, node string, loaded bool) {
	v := 0.0
	if loaded {
		v = 1.0
	}
	modelLoaded.WithLabelValues(model, node).Set(v)
}

// SetNodeUp marks a node as up (1) or down (0).
func SetNodeUp(node, hostname string, up bool) {
	v := 0.0
	if up {
		v = 1.0
	}
	nodeUp.WithLabelValues(node, hostname).Set(v)
}
