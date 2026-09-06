// Package router picks a backing engine for each inference request based on
// model placements across the cluster. It implements the engines.Engine
// interface so the rest of the codebase doesn't need to know whether a
// request is served locally or proxied to a worker.
//
// Selection policy:
//
//  1. If the local engine has the model loaded, use local (lowest latency).
//  2. Otherwise look up all worker nodes that have the model loaded.
//  3. Among those, pick the one with the fewest in-flight requests.
//  4. If no node has the model, fall through to local — the local engine
//     will return a "model not found" error which surfaces correctly.
//
// Remote engines reuse the vLLM driver (workers expose an OpenAI-compatible
// surface, just like vLLM/MLX). Engines are cached per node so we don't
// rebuild them on every request.
package router

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/store"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"
)

// tracer is package-scoped so spans created here all carry the same
// instrumentation-library name; the global TracerProvider (set in
// internal/controlplane/tracing.go) decides whether they're exported
// or no-op'd.
var tracer trace.Tracer = otel.Tracer("github.com/opod-io/opod/internal/router")

// FallbackChains carries the per-class fallback lists for a single
// primary model. Generic is the catalog's `fallback:`; the typed lists
// (ContextLength, ContentPolicy) come from `fallback_on_*:`. An empty
// typed list means "use Generic" — operators only fill the typed list
// when they want a class-specific target.
type FallbackChains struct {
	Generic       []string
	ContextLength []string
	ContentPolicy []string
}

// PickFor returns the chain that matches the given ErrorClass, falling
// back to Generic when the typed list is empty (the common case for
// most catalog entries).
func (c FallbackChains) PickFor(class ErrorClass) []string {
	switch class {
	case ClassContextLength:
		if len(c.ContextLength) > 0 {
			return c.ContextLength
		}
	case ClassContentPolicy:
		if len(c.ContentPolicy) > 0 {
			return c.ContentPolicy
		}
	}
	return c.Generic
}

// FallbackResolver returns the per-class fallback chains for a primary
// model. An empty FallbackChains value means "no fallback" — Router
// behaves exactly as it did before this hook existed. Typically backed
// by a closure over the catalog's `fallback*:` fields.
type FallbackResolver func(modelID string) FallbackChains

// Router implements engines.Engine by dispatching to either the local engine
// or a remote worker engine based on cluster placements.
type Router struct {
	local     engines.Engine
	store     store.Store
	localNode string // node id used for "local" placements (typically "local")

	// log emits structured fallback / pick events. Defaults to slog.Default()
	// when SetLogger isn't called; tests can swap in a discard logger.
	log *slog.Logger

	// maxFallbackAttempts caps the number of candidates the router will walk
	// before giving up. 0 means "no cap" (legacy behavior — walk the entire
	// chain). Set via SetMaxFallbackAttempts.
	maxFallbackAttempts int

	// heartbeatMaxAge declares how stale a worker's last heartbeat can be
	// before pick() refuses to route to it. 0 means "no check" (legacy).
	// Set via SetHeartbeatMaxAge.
	heartbeatMaxAge time.Duration

	// FallbackResolver is optional. When set, Chat / Embed will retry the
	// request against each fallback model in order on retriable errors
	// (anything Engine.Chat returns synchronously). Set via
	// router.SetFallbackResolver after construction.
	fallback FallbackResolver

	// localResolve maps a CATALOG model id to the LOCAL engine's native name
	// (e.g. ollama: "llama-3.2-1b" -> "llama3.2:1b"). The router routes by
	// catalog id because placements are catalog-keyed, so the native-name
	// translation must happen at DISPATCH and ONLY for the local engine —
	// remote workers receive the catalog id and resolve it against their own
	// engine. nil = identity (pass the catalog id through unchanged). Without
	// this, resolving to native BEFORE routing made pick() query placements by
	// the native name and never match a worker → fell through to local → 404.
	localResolve func(string) string

	// latency tracks per-model rolling p95 latency and (when the threshold
	// is non-zero) preempts a slow primary by trying a faster fallback
	// first. Always non-nil after New(); the threshold defaults to 0
	// (disabled — latencies are still recorded for traces / future
	// metrics, but no reordering). Also feeds `sort: latency|throughput`.
	latency *latencyStats

	// priceFn resolves a model id to its combined $/1K-token rate for
	// `sort: price` / `:floor`. nil disables price sorting (chain is
	// left in catalog order). Set via SetPriceResolver.
	priceFn func(modelID string) float64

	// Placement cooldown ("penalty box"): a worker node that errors
	// `placementAllowedFails` times in a row is parked for
	// `placementCooldownDur` so pick() skips it instead of routing
	// fresh requests to a flaky engine. Per-node, in-memory only;
	// reset on leader restart (the next request will re-prove the
	// node). Cooldown applies only to remote workers; the local
	// engine never enters cooldown (a flaky local engine is a
	// different operational problem).
	placementAllowedFails int
	placementCooldownDur  time.Duration

	// Sticky sessions: when stickyTTL > 0 the router pins a
	// (user_id, model) tuple to the worker that served its previous
	// successful request, so multi-turn chats reuse the same node's
	// KV cache. The entry refreshes on each successful pick and
	// expires after stickyTTL of inactivity. Bypassed for requests
	// with no user_id (`auth.KeyFrom(ctx) == nil` — typically dev
	// mode without keys) and for the synthetic `auto` model id (the
	// effective resolved model may change between turns).
	stickyTTL time.Duration

	// Hedging: when > 1 the router can fire a single request to
	// hedgeReplicas least-loaded workers concurrently and return
	// whichever responds first. Opt-in per request (via
	// Overrides.Hedge). 0 / 1 disables. Hard-capped at MaxHedgeReplicas
	// in the setter.
	hedgeReplicas int

	mu          sync.RWMutex
	inflight    map[string]int            // node_id → live request count
	inflightDim map[string]int            // node_id + "|" + model → live request count (drain waits)
	remotes     map[string]engines.Engine // node_id → cached remote engine
	cooldowns   map[string]time.Time      // node_id → time the node leaves the penalty box
	failures    map[string]int            // node_id → consecutive recent failures
	stickiness  map[string]stickyEntry    // user_id|model → pinned node + expiry

	// stickyInserts counts rememberSticky calls so the map can be swept
	// of expired entries opportunistically (every stickySweepEvery-th
	// insert, or whenever the map outgrows stickySweepThreshold).
	// Stickiness otherwise only deletes lazily in stickyPick, which
	// never fires for one-shot users — the map would grow unboundedly.
	stickyInserts int
}

// stickySweepEvery and stickySweepThreshold tune the opportunistic
// expired-entry sweep in rememberSticky. The sweep is O(map) under the
// write lock, so it runs at most once per stickySweepEvery inserts —
// unless the map has already grown past stickySweepThreshold, in which
// case it runs on every insert until expiry brings it back down.
const (
	stickySweepEvery     = 256
	stickySweepThreshold = 4096
)

// stickyEntry is one row of the per-(user_id, model) pin table.
type stickyEntry struct {
	NodeID    string
	ExpiresAt time.Time
}

// New constructs a Router that wraps the local engine and consults the store
// for placements + node info.
func New(local engines.Engine, st store.Store) *Router {
	return &Router{
		local:       local,
		store:       st,
		localNode:   "local",
		log:         slog.Default(),
		inflight:    make(map[string]int),
		inflightDim: make(map[string]int),
		remotes:     make(map[string]engines.Engine),
		cooldowns:   make(map[string]time.Time),
		failures:    make(map[string]int),
		stickiness:  make(map[string]stickyEntry),
		latency:     newLatencyStats(LatencyConfig{}),
	}
}

// MaxHedgeReplicas caps the per-request fan-out so a misconfigured
// client can't burn 50× the engine cost in one call. Three is enough
// to cut tail latency without the cost getting silly.
const MaxHedgeReplicas = 3

// SetHedgeReplicas enables request hedging. When a request opts in
// via Overrides.Hedge the router fires the call to the top-N
// least-loaded workers concurrently and returns whichever responds
// first; the losers' contexts are cancelled.
//
// n ≤ 1 disables hedging entirely. Values above MaxHedgeReplicas are
// silently clamped.
func (r *Router) SetHedgeReplicas(n int) {
	if n <= 1 {
		r.hedgeReplicas = 0
		return
	}
	if n > MaxHedgeReplicas {
		n = MaxHedgeReplicas
	}
	r.hedgeReplicas = n
}

// SetStickyTTL turns on per-(user_id, model) session stickiness with
// the given TTL. The router prefers the previously-picked worker for
// each tuple until the TTL elapses without activity, so multi-turn
// chats reuse the same node's KV cache.
//
// 0 (default) disables the feature — pick() behaves exactly as before.
// Recommended range: 60s–600s. Too low and the cache benefit
// disappears between turns; too high and load can stay lopsided after
// a session ends.
func (r *Router) SetStickyTTL(d time.Duration) {
	if d >= 0 {
		r.stickyTTL = d
	}
}

// SetPlacementCooldown configures the per-node circuit-breaker. After
// `allowedFails` consecutive engine errors from the same worker, pick()
// skips the node for `cooldown` before retrying. A single success after
// cooldown expires resets the counter.
//
// Both values must be > 0 to enable the feature. Either zero (the
// default) disables it — pick() behaves exactly as before.
func (r *Router) SetPlacementCooldown(allowedFails int, cooldown time.Duration) {
	if allowedFails < 0 || cooldown < 0 {
		return
	}
	r.placementAllowedFails = allowedFails
	r.placementCooldownDur = cooldown
}

// SetLogger swaps in a structured logger for fallback + pick events.
// Defaults to slog.Default() if never called.
func (r *Router) SetLogger(l *slog.Logger) {
	if l != nil {
		r.log = l
	}
}

// SetMaxFallbackAttempts caps how many candidates Chat/Embed will try
// before giving up. 0 (default) walks the entire chain.
func (r *Router) SetMaxFallbackAttempts(n int) {
	if n >= 0 {
		r.maxFallbackAttempts = n
	}
}

// SetHeartbeatMaxAge causes pick() to refuse to dispatch to a worker
// whose last heartbeat is older than `d`. 0 (default) disables the check.
// Useful for catching dead workers before the engine call timeout fires.
func (r *Router) SetHeartbeatMaxAge(d time.Duration) {
	if d >= 0 {
		r.heartbeatMaxAge = d
	}
}

// SetFallbackResolver wires a fallback chain provider into the router.
// Callers typically pass a closure over the catalog; nil disables fallback.
// SetLocalResolver installs the catalog-id → local-engine-native-name mapping
// used at dispatch when the router picks the local engine. Set from the API
// handler's ResolveModel so the router can route by catalog id yet still hand
// the local engine a name it understands.
func (r *Router) SetLocalResolver(f func(string) string) { r.localResolve = f }

func (r *Router) SetFallbackResolver(f FallbackResolver) {
	r.fallback = f
}

// SetLatencyConfig configures latency-aware fallback (Bet #1). With a
// non-zero P95Threshold, when a primary model's recent p95 latency
// exceeds the threshold, the router walks the catalog fallback chain
// for a faster candidate to try FIRST. Original primary stays in the
// chain so a fast-but-degraded fallback isn't a permanent demotion.
func (r *Router) SetLatencyConfig(cfg LatencyConfig) {
	r.latency = newLatencyStats(cfg)
}

// resolveChain returns [primary, ...generic fallback]. Kept for the
// existing call paths (tests, latency reorder) that want a pre-built
// list. For class-aware routing the call sites use chainsFor + PickFor
// after classifying the primary's failure.
//
// When no resolver is set or the model has no fallback entry, returns
// just [primary]. Bounded by SetMaxFallbackAttempts when configured.
func (r *Router) resolveChain(model string) []string {
	chains := r.chainsFor(model)
	return r.applyCap(buildChain(model, chains.Generic))
}

// chainsFor returns the typed fallback chains for `model`. A zero
// FallbackChains value (no resolver configured / no chain declared) is
// returned as-is.
func (r *Router) chainsFor(model string) FallbackChains {
	if r.fallback == nil {
		return FallbackChains{}
	}
	return r.fallback(model)
}

func buildChain(primary string, fb []string) []string {
	if len(fb) == 0 {
		return []string{primary}
	}
	out := make([]string, 0, len(fb)+1)
	out = append(out, primary)
	out = append(out, fb...)
	return out
}

// applyCap trims `chain` to MaxFallbackAttempts+1 candidates (primary
// + N fallbacks). The legacy behavior (limit=0) walks the entire chain.
func (r *Router) applyCap(chain []string) []string {
	if limit := r.maxFallbackAttempts; limit > 0 && len(chain) > limit+1 {
		chain = chain[:limit+1]
		metrics.ObserveRouterFallback("chain", "cap-exhausted")
	}
	return chain
}

// Name reports the underlying local engine name so /readyz and logs stay
// useful in single-node deployments.
func (r *Router) Name() string { return r.local.Name() }

// Endpoint reports the local engine endpoint.
func (r *Router) Endpoint() string { return r.local.Endpoint() }

// Health checks the local engine (workers' health is checked separately via
// the heartbeat loop).
func (r *Router) Health(ctx context.Context) error { return r.local.Health(ctx) }

// List returns the local engine's model list. (Cluster-wide listing happens
// via the placements store and the admin API.)
func (r *Router) List(ctx context.Context) ([]string, error) { return r.local.List(ctx) }

// Pull, Delete operate on the local engine. Workers are pulled-to via their
// own `opod model add` invocations.
func (r *Router) Pull(ctx context.Context, modelID string, onProgress func(string, int64, int64)) error {
	return r.local.Pull(ctx, modelID, onProgress)
}

func (r *Router) Delete(ctx context.Context, modelID string) error {
	return r.local.Delete(ctx, modelID)
}

// Unload forwards to the local engine. Cluster-wide unload (every remote
// holding a shard) isn't implemented yet — sharded models already tear
// down via the orchestrator's process-stop path on the workers.
func (r *Router) Unload(ctx context.Context, modelID string) error {
	return r.local.Unload(ctx, modelID)
}

// Embed dispatches an embedding request, with optional fallback. Tries the
// primary model first; on retriable error, walks the fallback chain in
// order. If every candidate fails, returns the PRIMARY's error since that's
// what the operator actually asked for.
//
// Per-request overrides (router.WithOverrides on the ctx) take precedence
// over the catalog chain: a non-empty Overrides.Fallbacks replaces the
// catalog chain entirely, and Overrides.NumRetries wraps each attempt in
// an exponential-backoff loop before advancing to the next candidate.
