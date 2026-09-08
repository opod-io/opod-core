// Package controlplane wires together the gateway, control-plane HTTP routes,
// and protocol adapters. It owns the chi router and the *http.Server lifecycle.
package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/api"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/cache"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/events"
	"github.com/opod-io/opod/internal/lifecycle"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/router"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
)

// Server is the leader-side HTTP server.
type Server struct {
	cfg    *config.Config
	store  store.Store
	engine engines.Engine
	cat    []models.Entry
	log    *slog.Logger
	http   *http.Server

	router      *router.Router
	orch        *scheduler.Orchestrator
	lifecycle   *lifecycle.Manager
	openaiH     *api.Handler
	load        loadStats
	nodeLoad    sync.Map // node id → nodeLoadSample: the worker's engine load from its last heartbeat (build item 14)
	plan        planFileState
	authf       authFileState
	policy      policyFileState
	rateBuckets *api.BucketStore

	// bus fans out dashboard refresh events. /admin/v1/events streams
	// to subscribed dashboards; producers (addModel, deleteModel, etc.)
	// publish topic strings on state change.
	bus *events.Bus

	// Version is stamped into traces and the access log. Set by callers
	// before Start; defaults to "dev" if unset.
	Version string

	tracerShutdown func(context.Context) error
}

func NewServer(cfg *config.Config, st store.Store, eng engines.Engine, cat []models.Entry, log *slog.Logger, orch *scheduler.Orchestrator) *Server {
	routed := router.New(eng, st)

	// Wire catalog-driven fallback chains: when a request to model X fails,
	// retry against X's catalog fallback list in order. The resolver
	// returns the typed chains (generic + per-error-class) so the router
	// can pick the right list after classifying the primary's failure.
	// Closure captures the catalog slice — fresh lookups happen per call
	// so a catalog reload would be observed (catalog hot-reload isn't
	// shipped yet but this leaves room).
	routed.SetFallbackResolver(func(modelID string) router.FallbackChains {
		entry := models.FindByID(cat, modelID)
		if entry == nil {
			return router.FallbackChains{}
		}
		return router.FallbackChains{
			Generic:       entry.Fallback,
			ContextLength: entry.FallbackOnContextLength,
			ContentPolicy: entry.FallbackOnContentPolicy,
		}
	})
	// Price resolver for `sort: price` / `:floor` — combined prompt +
	// completion $/1K from the catalog + vendor pricing table. Free
	// local models return 0 and sort first.
	routed.SetPriceResolver(func(modelID string) float64 {
		pp, pc := models.PriceFor(modelID, cat)
		return pp + pc
	})
	// Latency-aware fallback (Bet #1): opt-in via router.latency_fallback_p95_seconds.
	// Zero (default) leaves behavior unchanged.
	if cfg.Router.LatencyFallbackP95Seconds > 0 {
		routed.SetLatencyConfig(router.LatencyConfig{
			P95Threshold: time.Duration(cfg.Router.LatencyFallbackP95Seconds) * time.Second,
		})
	}
	// Placement cooldown ("penalty box"): a worker that errors N times
	// in a row gets parked for the cooldown duration so pick() skips it.
	// Both knobs must be > 0 to enable.
	if cfg.Router.PlacementAllowedFails > 0 && cfg.Router.PlacementCooldownSeconds > 0 {
		routed.SetPlacementCooldown(
			cfg.Router.PlacementAllowedFails,
			time.Duration(cfg.Router.PlacementCooldownSeconds)*time.Second,
		)
	}
	// Sticky sessions: pin (user_id, model) to its last worker for the
	// TTL so multi-turn chats reuse KV cache. Disabled when ttl == 0.
	if cfg.Router.StickySessionTTLSeconds > 0 {
		routed.SetStickyTTL(time.Duration(cfg.Router.StickySessionTTLSeconds) * time.Second)
	}
	// Request hedging — opt-in per request via opod.hedge.
	if cfg.Router.HedgeReplicas > 1 {
		routed.SetHedgeReplicas(cfg.Router.HedgeReplicas)
	}
	// Heartbeat reaper: never dispatch to a worker that stopped
	// heartbeating (dead pod, partitioned node). Default 30s (workers
	// heartbeat every 5s); heartbeat_max_age_seconds: 0 disables.
	if cfg.Router.HeartbeatMaxAgeSeconds > 0 {
		routed.SetHeartbeatMaxAge(time.Duration(cfg.Router.HeartbeatMaxAgeSeconds) * time.Second)
	}

	openaiH := &api.Handler{
		Engine:  routed,
		Store:   st,
		Catalog: cat,
		Default: cfg.Router.DefaultModel,
	}
	// The router routes by catalog id; hand it the local engine's catalog→native
	// resolver so a request picked to run on THIS leader's engine still gets a
	// name the engine understands. Remote workers resolve on their own side.
	routed.SetLocalResolver(func(catalogID string) string {
		native, err := openaiH.ResolveModel(catalogID)
		if err != nil {
			return catalogID
		}
		return native
	})
	buckets := api.NewBucketStore()
	// The request-path policy is one value the handler serves under (P13-8):
	// catalog (label bounding), rate-limit buckets, response cache; the
	// guardrail registry is swapped in by the policy-file watcher.
	openaiH.SetPolicy(&api.Policy{Catalog: cat, Buckets: buckets, Cache: buildResponseCache(cfg.Observability.ResponseCache, st, log)})
	return &Server{
		cfg:         cfg,
		store:       st,
		engine:      eng,
		cat:         cat,
		log:         log,
		router:      routed,
		orch:        orch,
		openaiH:     openaiH,
		rateBuckets: buckets,
		bus:         events.New(),
	}
}

// buildResponseCache instantiates the configured driver (memory or
// sqlite). Returns nil when disabled — the api package short-circuits
// the cache path on nil.
func buildResponseCache(cfg config.ResponseCacheConfig, st store.Store, log *slog.Logger) cache.Cache {
	if !cfg.Enabled {
		return nil
	}
	ttl := time.Duration(cfg.DefaultTTLSeconds) * time.Second
	switch cfg.Driver {
	case "", "memory":
		return cache.NewMemory(cfg.MaxEntries, ttl)
	case "sqlite":
		return cache.NewSQLite(st.Cache(), ttl)
	default:
		log.Warn("unknown response_cache driver — disabling cache", "driver", cfg.Driver)
		return nil
	}
}

func (s *Server) Start(ctx context.Context) error {
	s.StartPlanWatcher(ctx)
	s.StartAuthWatcher(ctx)
	s.StartPolicyWatcher(ctx)
	s.StartTrimmer(ctx)
	if s.Version == "" {
		s.Version = "dev"
	}
	// Init OTLP tracing (no-op if endpoint not configured).
	shutdown, err := initTracing(ctx, s.cfg.Observability.OTLPEndpoint, s.Version, s.log)
	if err != nil {
		return fmt.Errorf("init tracing: %w", err)
	}
	s.tracerShutdown = shutdown

	// Wrap chi router with otelhttp so each inbound request gets a span.
	// Even when tracing is disabled (NoopTracerProvider), the wrapper still
	// participates in W3C traceparent propagation — cheap.
	handler := otelhttp.NewHandler(s.routes(), "http.request",
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return r.Method + " " + r.URL.Path
		}),
	)

	s.http = &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	s.log.Info("listening", "addr", s.cfg.Listen)
	errCh := make(chan error, 1)
	go func() { errCh <- s.http.ListenAndServe() }()
	select {
	case <-ctx.Done():
		return s.Shutdown(context.Background())
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			return fmt.Errorf("listen: %w", err)
		}
		return nil
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	httpErr := s.http.Shutdown(ctx)
	if s.tracerShutdown != nil {
		// Best-effort: flush any pending spans. Don't mask the http shutdown
		// error if both fail.
		if err := s.tracerShutdown(ctx); err != nil {
			s.log.Warn("tracer shutdown", "err", err)
		}
	}
	return httpErr
}

func (s *Server) routes() http.Handler {
	r := chi.NewRouter()
	// Recoverer first: a panic anywhere downstream is caught, and the
	// accessLog middleware can still record the 500.
	r.Use(middleware.Recoverer)
	r.Use(middleware.RequestID)
	// Stash the kernel-reported peer address BEFORE RealIP rewrites
	// RemoteAddr from forwarding headers — bootstrapAdminKey must check
	// loopback against the real TCP peer, not a spoofable header.
	r.Use(stashRemoteAddr)
	r.Use(middleware.RealIP)
	r.Use(s.accessLog)

	// Public
	r.Get("/healthz", s.healthz)
	r.Get("/readyz", s.readyz)
	r.Get("/loadz", s.loadz)
	r.Handle("/metrics", promhttp.Handler())

	// No dashboard in core (ADR-022): "/" is a 404. The product console lives in the control plane.

	// OpenAI-compatible + Anthropic-compatible (auth + quota)
	r.Route("/v1", func(r chi.Router) {
		// Load accounting first so /loadz sees every inference request,
		// including ones a later middleware rejects.
		r.Use(s.trackLoad)
		// Cap request bodies first so nothing downstream (rate-limit
		// estimation, the dispatch handlers) buffers an unbounded body.
		r.Use(s.limitRequestBody)
		r.Use(auth.MiddlewareFn(s.store.APIKeys(), s.requireKeys))
		// Per-key model allowlist runs BEFORE quota: a key with no quota
		// to spend on an unauthorized model would otherwise burn a 429
		// instead of the more accurate 403.
		r.Use(api.ModelAllowMiddleware(s.store))
		// RPM/TPM ceilings. Wired before the daily quota check so a
		// runaway client gets the more-actionable 429 with Retry-After
		// instead of the daily 429.
		r.Use(api.RateLimitMiddleware(s.rateBuckets))
		// Stamp a request id + standard rate-limit headers on every
		// response so client SDKs see throttling status without
		// special-casing Opod. Runs after RateLimitMiddleware so the
		// remaining-* values include this request's deduction (the
		// contract documented on ResponseHeadersMiddleware).
		r.Use(api.ResponseHeadersMiddleware(s.rateBuckets))
		r.Use(api.QuotaMiddleware(s.store))
		r.Get("/models", s.openaiH.ListModels)
		r.Post("/chat/completions", s.dispatchOpenAIChat)
		r.Post("/embeddings", s.openaiH.Embeddings)
		// OpenAI chat + embeddings are the whole protocol surface (ADR-022 step 4,
		// 2026-09-07: Anthropic Messages, audio and rerank left core).
	})

	// Admin (admin-only)
	r.Route("/admin/v1", func(r chi.Router) {
		// Cap bodies first — every admin handler json.Decodes r.Body, and a
		// node-scope token (reachable via /nodes/register + /heartbeat) could
		// otherwise stream an unbounded body. Mirrors the /v1 group.
		r.Use(s.limitRequestBody)
		// The admin surface ALWAYS needs a key (the seeded manager token, the
		// local admin key, or a node token): requireKeys only gates the
		// gateway. Before P12-3 the keyless dev shortcut left /admin/v1 open
		// on every managed leader whose endpoint had keys optional.
		r.Use(auth.MiddlewareFn(s.store.APIKeys(), func() bool { return true }))
		r.Use(s.auditMiddleware)

		// Node lifecycle endpoints accept either admin or node scope so
		// agents can register and heartbeat with a scope=node token.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireScopeAny("admin", "node"))
			r.Post("/nodes/register", s.registerNode)
			r.Post("/nodes/heartbeat", s.heartbeatNode)
		})

		// Everything else is admin only.
		r.Group(func(r chi.Router) {
			r.Use(auth.RequireScope("admin"))

			// Nodes
			r.Get("/nodes", s.listNodes)
			r.Post("/nodes/{id}/drain", s.drainNode)
			r.Post("/nodes/{id}/sleep", s.sleepWorker)   // sleep tier (build item 13)
			r.Post("/nodes/{id}/resume", s.resumeWorker) // wake it
			r.Delete("/nodes/{id}", s.deleteNode)

			// Models
			r.Get("/models", s.listInstalledModels)
			r.Get("/catalog", s.listCatalog)
			r.Post("/models", s.addModel)
			r.Delete("/models/{id}", s.deleteModel)
			r.Post("/models/{id}/unload", s.unloadModel)
			r.Post("/models/{id}/load", s.loadModel)

			// Memory: live engine residency + the desired-placement set.
			r.Get("/memory", s.memoryStatus)

			// Tokens
			r.Get("/tokens", s.listTokens)
			r.Post("/tokens", s.createToken)
			r.Patch("/tokens/{id}", s.editToken)
			r.Delete("/tokens/{id}", s.revokeToken)

			// Observability
			r.Get("/usage/stream", s.usageStream)
			r.Get("/events/stream", s.eventLogStream)

			// Shards
			r.Get("/shards", s.listShards)
			r.Get("/shards/processes", s.listShardProcesses)
			r.Post("/shards/create", s.createShards)
			r.Delete("/shards/{model_id}", s.deleteShards)

			// Config (read-only sanitized view)
			// stable manager surface (contract.go): discovery of what this leader speaks
			r.Get("/version", s.adminVersion)
			r.Get("/capabilities", s.adminCapabilities)

			r.Get("/config", s.getConfig)

			// Routing chain — the ordered list of model ids walked for
			// model="auto" / fallback. GET returns the stored chain (or the
			// computed default); PUT replaces it; DELETE resets to default.

			// Compact status used by the dashboard top-bar chips. Same
			// data the `opod status` CLI surfaces, returned as one JSON
			// blob so the UI can poll a single endpoint.
			r.Get("/status", s.statusSummary)

			// Server-Sent Events stream. Dashboards subscribe once and
			// re-fetch the relevant view on every event. Replaces the
			// per-tab 5 s polling with push-on-change.
			r.Get("/events", s.eventsStream)

			// Onboarding-and-sharing (M3-T23 / M3-T24 / M3-T26)
			r.Post("/healthcheck", s.healthcheck)

			// Observability callbacks — list configured sinks +
			// fire a synthetic test event.

			// Response cache stats + flush.
			r.Get("/cache/stats", s.cacheStats)
			r.Delete("/cache", s.cacheFlush)
		})
	})

	return r
}

// realRemoteAddrKey carries the pre-RealIP RemoteAddr on the request
// context. Unexported struct type — no collision with other packages.
type realRemoteAddrKey struct{}

// stashRemoteAddr records the kernel-reported peer address before
// middleware.RealIP rewrites RemoteAddr from True-Client-IP /
// X-Real-IP / X-Forwarded-For. Loopback-gated handlers must trust only
// this value.
func stashRemoteAddr(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), realRemoteAddrKey{}, r.RemoteAddr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// realRemoteAddr returns the TCP peer address stashed by
// stashRemoteAddr, falling back to r.RemoteAddr when the middleware
// didn't run (e.g. handlers exercised directly in tests).
func realRemoteAddr(r *http.Request) string {
	if v, ok := r.Context().Value(realRemoteAddrKey{}).(string); ok && v != "" {
		return v
	}
	return r.RemoteAddr
}

// defaultMaxBodyBytes caps /v1/* request bodies at 32 MiB — generous
// for chat/embedding payloads (vision requests inline base64 images)
// while keeping a single request from buffering unbounded memory.
// Override via `max_body_bytes` in config.yaml / OPOD_MAX_BODY_BYTES.
const defaultMaxBodyBytes = 32 << 20

// limitRequestBody wraps every /v1 request body in http.MaxBytesReader
// so downstream io.ReadAll calls fail fast at the cap instead of
// buffering whatever a client streams at us.
func (s *Server) limitRequestBody(next http.Handler) http.Handler {
	limit := s.cfg.MaxBodyBytes
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

// dispatchOpenAIChat inspects the request body's "model" field. If it names
// a vendor model (claude-*, gpt-*) AND fallback is configured, the request is
// proxied to the vendor; otherwise it goes to the local engine.
