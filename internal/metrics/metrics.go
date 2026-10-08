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

	// Time to the first token a client can use, on streamed answers only
	// (R15.13). A non-streamed answer has no first token to wait for and is
	// never observed here, so the histogram is not diluted by whole-response
	// latencies that would make the wait look longer than it is.
	ttftSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opod_time_to_first_token_seconds",
		Help:    "Time to the first usable token of a streamed answer, by model.",
		Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"model"})

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

// ObserveTTFT records one streamed answer's time to its first usable token.
// Zero is never recorded: it means "not streamed", not "instant".
func ObserveTTFT(model string, d time.Duration) {
	if d <= 0 {
		return
	}
	ttftSeconds.WithLabelValues(model).Observe(d.Seconds())
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

var (
	admissionHeldTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_admission_held_total",
		Help: "Requests that found no capacity and were handled by the admission hold (ADR-082), by outcome: served (capacity came back inside the budget), shed_deadline (503 at the budget), shed_cap (503 at once: too many already held), shed_evicted (503: shed to make room for a higher request class, ADR-086), cancelled (the client went away).",
	}, []string{"outcome"})
	admissionHeld = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_admission_held",
		Help: "Requests held right now waiting for capacity, by request class (ADR-082, ADR-086). Without classes in the policy every held request is standard.",
	}, []string{"class"})
	admissionShedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_admission_shed_total",
		Help: "Held requests shed with 503 + Retry-After, by request class and reason: deadline (the class's budget ran out), cap (the hold or the class was full), evicted (shed to make room for a higher class) (ADR-086), unavailable (every worker stopped serving while it waited for a slot with no budget of its class, ADR-091).",
	}, []string{"class", "reason"})
	admissionWaitedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "opod_admission_slot_waits_total",
		Help: "Requests dispatched only after they waited for a free worker slot (ADR-091), by request class: the leader held them while every slot their class may use was busy.",
	}, []string{"class"})
	// The wait itself (ADR-091), from the moment the request reached the
	// gate to the slot it was granted — 0 for one that found a slot free. Every
	// admitted request is observed, so _count is the admitted requests and a
	// percentile is over all of them; a shed request is not (it is counted in
	// opod_admission_shed_total). The time to first token includes this wait.
	admissionWaitSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "opod_admission_wait_seconds",
		Help:    "Seconds an admitted request waited at the admission gate for a worker slot (ADR-091), by request class; 0 when a slot was free.",
		Buckets: []float64{0.001, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10, 30, 60},
	}, []string{"class"})
	admissionInUse = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_admission_in_flight",
		Help: "Requests the admission gate has dispatched and not yet seen complete, by model and request class (ADR-091). Compare with opod_admission_slots.",
	}, []string{"model", "class"})
	admissionSlots = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_admission_slots",
		Help: "Worker slots a model can serve at once across the workers that take its requests now (ADR-091); 0 while none can serve. Ungoverned workers add none: see opod_admission_ungoverned_workers.",
	}, []string{"model"})
	admissionUngoverned = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_admission_ungoverned_workers",
		Help: "Workers (or gangs) serving a model that report no slot count (ADR-091). While any is serving, the model's admission is unbounded: requests are held only when no worker can serve, and classes rank nothing on a busy endpoint.",
	}, []string{"model"})
	workerSlots = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "opod_worker_slots",
		Help: "Requests a worker's engine serves at once, as its heartbeat reports it (ADR-091); compare with opod_router_inflight for the same node. Absent for a worker that reports none (ungoverned).",
	}, []string{"node"})
)

// ObserveAdmissionSlotWait records one request dispatched after waiting for a slot.
func ObserveAdmissionSlotWait(class string) { admissionWaitedTotal.WithLabelValues(class).Inc() }

// ObserveAdmissionWait records how long one admitted request waited for its slot.
func ObserveAdmissionWait(class string, d time.Duration) {
	admissionWaitSeconds.WithLabelValues(class).Observe(max(d, 0).Seconds())
}

// SetAdmissionInUse sets the gate's dispatched-not-completed count of one class.
func SetAdmissionInUse(model, class string, n int) {
	admissionInUse.WithLabelValues(model, class).Set(float64(n))
}

// SetAdmissionCapacity sets a model's governed slots and its ungoverned workers.
func SetAdmissionCapacity(model string, slots, ungoverned int) {
	admissionSlots.WithLabelValues(model).Set(float64(slots))
	admissionUngoverned.WithLabelValues(model).Set(float64(ungoverned))
}

// ForgetAdmissionModel drops a model's admission series (its pool is gone).
func ForgetAdmissionModel(model string) {
	admissionSlots.DeleteLabelValues(model)
	admissionUngoverned.DeleteLabelValues(model)
	admissionInUse.DeletePartialMatch(prometheus.Labels{"model": model})
}

// SetWorkerSlots sets one worker's slot count; n ≤ 0 removes the series.
func SetWorkerSlots(node string, n int) {
	if n <= 0 {
		workerSlots.DeleteLabelValues(node)
		return
	}
	workerSlots.WithLabelValues(node).Set(float64(n))
}

// ObserveAdmissionHold records how one held request ended.
func ObserveAdmissionHold(outcome string) { admissionHeldTotal.WithLabelValues(outcome).Inc() }

// ObserveAdmissionShed records one held request shed, by class and reason.
func ObserveAdmissionShed(class, reason string) {
	admissionShedTotal.WithLabelValues(class, reason).Inc()
}

// SetAdmissionHeld sets the number of requests of class held right now.
func SetAdmissionHeld(class string, n int64) { admissionHeld.WithLabelValues(class).Set(float64(n)) }
