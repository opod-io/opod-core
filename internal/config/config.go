// Package config loads Opod configuration from YAML and environment.
//
// Precedence (lowest → highest): defaults → YAML file → environment variables.
// All fields have sensible defaults; no config file is required to run.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the full runtime configuration for a Opod node.
type Config struct {
	Listen      string `yaml:"listen"`
	ExternalURL string `yaml:"external_url"`
	DataDir     string `yaml:"data_dir"`
	LogLevel    string `yaml:"log_level"`
	CatalogDir  string `yaml:"catalog_dir"`
	// MaxBodyBytes caps the request body size on the /v1/* API surface.
	// 0 (default) uses the server's built-in 32 MiB ceiling. Env
	// override: OPOD_MAX_BODY_BYTES.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// TrustProxyHeaders controls whether X-Forwarded-Host / X-Forwarded-Proto
	// are honored when deriving the public base URL embedded in invite /
	// connect cards. Off by default: a spoofed header could otherwise mint a
	// share card pointing recipients at an attacker host (token phishing).
	// Enable only when Opod sits behind a trusted reverse proxy. Env
	// override: OPOD_TRUST_PROXY_HEADERS.
	TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
	// BlockPrivateTargets, when true, refuses outbound connections from the
	// observability callback / guardrail / catalog-probe HTTP clients to
	// loopback, link-local, and RFC-1918 private addresses — defense in depth
	// against SSRF when those URLs come from a less-trusted source. Off by
	// default because on-prem webhooks at private addresses are common. Env
	// override: OPOD_BLOCK_PRIVATE_TARGETS.
	BlockPrivateTargets bool                `yaml:"block_private_targets"`
	Storage             StorageConfig       `yaml:"storage"`
	Auth                AuthConfig          `yaml:"auth"`
	Engine              EngineConfig        `yaml:"engine"`
	Router              RouterConfig        `yaml:"router"`
	Observability       ObservabilityConfig `yaml:"observability"`
	Placement           PlacementConfig     `yaml:"placement"`
	// Surfaces switches product surfaces off for a leader run by an external
	// manager (ADR-022 / P11-2). Defaults keep every surface on for a
	// standalone `opod up`; the switches never touch the request-path
	// mechanisms or the stable admin surface (controlplane/contract.go).
	Surfaces SurfacesConfig `yaml:"surfaces"`
}

// SurfacesConfig — each field has an env override a manager can set on the
// process without a config file:
//
//	OPOD_UI              accepted for compatibility; core has no dashboard since ADR-022 ("/" is always 404)
//	OPOD_EGRESS=off      never forward to a cloud vendor, whatever keys are in the env
//	OPOD_CALLBACKS=off   no webhook / Langfuse / S3 sinks, no /admin/v1/callbacks
//	OPOD_MANAGED=1       run by a manager: update check off, banner says so
type SurfacesConfig struct {
	UI        bool `yaml:"ui"`
	Egress    bool `yaml:"egress"`
	Callbacks bool `yaml:"callbacks"`
	Managed   bool `yaml:"managed"`
}

// Summary is the one-line banner form: "ui off · egress off · callbacks off · managed".
func (s SurfacesConfig) Summary() string {
	on := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	out := "ui " + on(s.UI) + " · egress " + on(s.Egress) + " · callbacks " + on(s.Callbacks)
	if s.Managed {
		out += " · managed"
	}
	return out
}

// offSwitch: "off", "0", "false", "no" (any case) mean off.
func offSwitch(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no":
		return true
	}
	return false
}

// PlacementConfig tunes the memory-lifecycle manager (admission,
// evict-and-swap) for this node's local engine.
type PlacementConfig struct {
	// Exclusive enforces one resident chat model per machine: loading a
	// model evicts every other non-pinned resident model first, not just
	// enough to fit. Env override: OPOD_EXCLUSIVE=1.
	Exclusive bool `yaml:"exclusive"`
	// ReservePercent of total RAM is held back from the admission budget
	// for the OS and engine overhead. Default 20. Env override:
	// OPOD_PLACEMENT_RESERVE_PERCENT.
	ReservePercent int `yaml:"reserve_percent"`
	// DrainTimeoutSeconds bounds how long an eviction waits for in-flight
	// requests to finish before unloading anyway. Default 30.
	// Env override: OPOD_PLACEMENT_DRAIN_TIMEOUT_SECONDS.
	DrainTimeoutSeconds int `yaml:"drain_timeout_seconds"`
}

type StorageConfig struct {
	Type      string `yaml:"type"`
	DSN       string `yaml:"dsn"`
	ModelsDir string `yaml:"models_dir"`
}

type AuthConfig struct {
	RequireKeys bool `yaml:"require_keys"`
	// AdminToken, when set, is seeded as an ADMIN-scoped API key at every
	// `opod up` (idempotent, like JoinToken below). It lets an external
	// manager that provisioned the leader (e.g. a control plane that put
	// the token in a Secret) call /admin/v1 without extracting the
	// auto-generated admin.key from the leader's data dir. Env override:
	// OPOD_ADMIN_TOKEN.
	AdminToken string `yaml:"admin_token"`
	// JoinToken, when set, is a node-scoped API key the leader seeds into its DB
	// on every `opod up` (idempotent). It makes worker joins survive a fresh/wiped
	// leader DB: any leader started with the same JoinToken accepts workers holding
	// it, instead of only the single leader whose DB happens to contain a
	// hand-minted node token.
	//
	// EMPTY BY DEFAULT — there is NO built-in shared secret. A leader with no
	// JoinToken mints node tokens the normal way (`opod token create --node`), so
	// nothing is joinable without an operator-issued credential. To get the
	// zero-config "any matching leader accepts this worker" behaviour on a trusted
	// LAN / tailscale cluster, set this field (or OPOD_JOIN_TOKEN) to a private
	// value you choose — a control plane does this by putting a token in a Secret.
	JoinToken string `yaml:"join_token"`
}

type EngineConfig struct {
	Preferred        string `yaml:"preferred"`
	OllamaEndpoint   string `yaml:"ollama_endpoint"`
	VLLMEndpoint     string `yaml:"vllm_endpoint"`
	VLLMAPIKey       string `yaml:"-" json:"-"` // populated from VLLM_API_KEY env
	MLXEndpoint      string `yaml:"mlx_endpoint"`
	LlamaCppEndpoint string `yaml:"llamacpp_endpoint"`
}

type RouterConfig struct {
	DefaultModel string `yaml:"default_model"`
	// PullDefaultModel controls whether the leader downloads default_model into its
	// OWN local engine at startup. Default FALSE: the leader is ROUTER-ONLY and
	// loads NO model locally unless the operator opts in (pull_default_model: true /
	// OPOD_PULL_DEFAULT_MODEL=1). It still uses default_model for routing/fallback,
	// but a GPU-less leader (e.g. a Mac fronting GPU workers) never re-pulls a model
	// on every `opod up`. Models live on workers; the leader just routes.
	PullDefaultModel bool `yaml:"pull_default_model"`
	// StickySessions is the legacy boolean. Now superseded by
	// StickySessionTTLSeconds — keep parseable for old configs but the
	// new field is the source of truth.
	StickySessions          bool `yaml:"sticky_sessions"`
	StickySessionTTLSeconds int  `yaml:"sticky_session_ttl_seconds"`

	// LatencyFallbackP95Seconds enables ROADMAP Bet #1 (latency-aware
	// fallback). When the rolling p95 latency for a primary model exceeds
	// this many seconds, the router walks the catalog fallback chain for
	// a faster candidate to try FIRST. Zero (default) keeps the historical
	// failure-only behavior. Common values: 5–10 seconds. Env override:
	// OPOD_LATENCY_P95_SECONDS.
	LatencyFallbackP95Seconds int `yaml:"latency_fallback_p95_seconds"`

	// PlacementAllowedFails + PlacementCooldownSeconds together drive the
	// per-node circuit breaker. After this many consecutive engine
	// errors from a worker, the router parks the node for the configured
	// cooldown. Both must be > 0 to enable the feature; either zero
	// disables it. Env overrides: OPOD_PLACEMENT_ALLOWED_FAILS,
	// OPOD_PLACEMENT_COOLDOWN_SECONDS.
	PlacementAllowedFails    int `yaml:"placement_allowed_fails"`
	PlacementCooldownSeconds int `yaml:"placement_cooldown_seconds"`

	// HeartbeatMaxAgeSeconds makes the router refuse to dispatch to a
	// worker whose last heartbeat is older than this — the reaper that
	// keeps requests off a worker whose pod died and re-joined under a
	// new identity. Workers heartbeat every 5s; default 30. Zero
	// disables. Env override: OPOD_HEARTBEAT_MAX_AGE_SECONDS.
	HeartbeatMaxAgeSeconds int `yaml:"heartbeat_max_age_seconds"`

	// HedgeReplicas, when > 1, enables request hedging — the router
	// can fire a single request to N least-loaded workers
	// concurrently and return whichever responds first. Each request
	// opts in individually via `opod.hedge: true` body field or
	// `X-Opod-Hedge: 1` header. Cap is router.MaxHedgeReplicas.
	HedgeReplicas int `yaml:"hedge_replicas"`
}

// ObservabilityConfig holds knobs for traces/logs/metrics integrations
// that aren't on by default. The Prometheus /metrics endpoint is always
// on — this struct is for the optional extras.
type ObservabilityConfig struct {
	// OTLPEndpoint is the OTLP/HTTP collector URL (e.g.
	// http://localhost:4318). Empty → tracing disabled (NoopTracerProvider,
	// zero overhead). Set via OPOD_OTLP_ENDPOINT env or this YAML key.
	OTLPEndpoint string `yaml:"otlp_endpoint"`

	// Callbacks ship usage / audit / fallback events to external
	// observability sinks (webhooks, Langfuse, etc.). Each entry runs
	// in its own goroutine with a bounded queue — a slow receiver
	// can't stall the gateway. Drops on overflow are counted via
	// opod_callback_sent_total{outcome="dropped"}.

	// Guardrails run synchronously on the request path. Each entry
	// chooses a mode (pre | post | logging_only) and a driver (today:
	// `webhook`). Pre guardrails can rewrite or block the request
	// before the engine sees it; logging_only entries observe without
	// intervening. Post mode is reserved for a follow-up — see the
	// CHANGELOG for the streaming-response design limitation.

	// ResponseCache stores deterministic responses (embeddings today;
	// chat completions to follow). Disabled when Enabled=false.
	ResponseCache ResponseCacheConfig `yaml:"response_cache"`
}

// ResponseCacheConfig configures the response cache.
type ResponseCacheConfig struct {
	Enabled           bool   `yaml:"enabled"`
	Driver            string `yaml:"driver"`              // memory | sqlite
	MaxEntries        int    `yaml:"max_entries"`         // memory only; 0 = 1000
	DefaultTTLSeconds int    `yaml:"default_ttl_seconds"` // 0 = 24h
}

// Default returns a Config populated with safe defaults for a single-node setup.
func Default() *Config {
	home, _ := os.UserHomeDir()
	dataDir := filepath.Join(home, ".opod")
	return &Config{
		Listen:      ":8080",
		ExternalURL: "",
		Surfaces:    SurfacesConfig{UI: true, Egress: true, Callbacks: true},
		DataDir:     dataDir,
		LogLevel:    "info",
		CatalogDir:  "", // empty → use built-in catalog dir resolution
		Storage: StorageConfig{
			Type:      "sqlite",
			DSN:       filepath.Join(dataDir, "state.db"),
			ModelsDir: filepath.Join(dataDir, "models"),
		},
		Auth: AuthConfig{
			RequireKeys: true,
			JoinToken:   "", // no built-in shared secret; opt in via OPOD_JOIN_TOKEN / auth.join_token
		},
		Engine: EngineConfig{
			Preferred:        "ollama",
			OllamaEndpoint:   "http://127.0.0.1:11434",
			VLLMEndpoint:     "http://127.0.0.1:8000",
			MLXEndpoint:      "http://127.0.0.1:8080",
			LlamaCppEndpoint: "http://127.0.0.1:8089",
		},
		Router: RouterConfig{
			DefaultModel:           "",
			StickySessions:         true,
			HeartbeatMaxAgeSeconds: 30,
		},
	}
}

// Load reads config from path (if it exists), then overlays environment variables.
// If path is empty, ~/.opod/config.yaml is tried.
func Load(path string) (*Config, error) {
	cfg := Default()
	// Pristine copy of the defaults so we can tell "still the eagerly
	// derived default" from "explicitly configured" after the overlays.
	defaults := Default()

	if path == "" {
		path = filepath.Join(cfg.DataDir, "config.yaml")
	}

	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	applyEnv(cfg)
	// Default() derives storage paths from the default data dir eagerly,
	// so a data_dir override (YAML or OPOD_DATA_DIR) would otherwise
	// leave them pointing at the old location. Re-derive any storage
	// path that wasn't itself overridden from the final DataDir.
	if cfg.DataDir != defaults.DataDir {
		if cfg.Storage.DSN == defaults.Storage.DSN {
			cfg.Storage.DSN = filepath.Join(cfg.DataDir, "state.db")
		}
		if cfg.Storage.ModelsDir == defaults.Storage.ModelsDir {
			cfg.Storage.ModelsDir = filepath.Join(cfg.DataDir, "models")
		}
	}
	expand(cfg)

	if err := ensureDirs(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Save writes the config back to YAML at path (or default location).
func (c *Config) Save(path string) error {
	if path == "" {
		path = filepath.Join(c.DataDir, "config.yaml")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	return os.WriteFile(path, data, 0o600)
}

func applyEnv(c *Config) {
	if v := os.Getenv("OPOD_UI"); v != "" {
		c.Surfaces.UI = !offSwitch(v)
	}
	if v := os.Getenv("OPOD_EGRESS"); v != "" {
		c.Surfaces.Egress = !offSwitch(v)
	}
	if v := os.Getenv("OPOD_CALLBACKS"); v != "" {
		c.Surfaces.Callbacks = !offSwitch(v)
	}
	if v := os.Getenv("OPOD_MANAGED"); v != "" {
		c.Surfaces.Managed = !offSwitch(v)
	}
	if v := os.Getenv("OPOD_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("OPOD_DATA_DIR"); v != "" {
		c.DataDir = v
	}
	if v := os.Getenv("OPOD_STORAGE_DSN"); v != "" {
		c.Storage.DSN = v
	}
	if v := os.Getenv("OPOD_MAX_BODY_BYTES"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
			c.MaxBodyBytes = n
		}
	}
	if v := os.Getenv("OPOD_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("OPOD_EXTERNAL_URL"); v != "" {
		c.ExternalURL = v
	}
	if v := os.Getenv("OPOD_TRUST_PROXY_HEADERS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.TrustProxyHeaders = b
		}
	}
	if v := os.Getenv("OPOD_BLOCK_PRIVATE_TARGETS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.BlockPrivateTargets = b
		}
	}
	if v := os.Getenv("OPOD_OLLAMA_ENDPOINT"); v != "" {
		c.Engine.OllamaEndpoint = v
	}
	if v := os.Getenv("OPOD_VLLM_ENDPOINT"); v != "" {
		c.Engine.VLLMEndpoint = v
	}
	// OPOD_VLLM_API_KEY is the opod-specific name; VLLM_API_KEY is read as a
	// deprecated fallback so existing setups keep working. The prefixed form
	// wins when both are set.
	if v := os.Getenv("VLLM_API_KEY"); v != "" {
		c.Engine.VLLMAPIKey = v
	}
	if v := os.Getenv("OPOD_VLLM_API_KEY"); v != "" {
		c.Engine.VLLMAPIKey = v
	}
	if v := os.Getenv("OPOD_MLX_ENDPOINT"); v != "" {
		c.Engine.MLXEndpoint = v
	}
	if v := os.Getenv("OPOD_LLAMACPP_ENDPOINT"); v != "" {
		c.Engine.LlamaCppEndpoint = v
	}
	if v := os.Getenv("OPOD_ENGINE"); v != "" {
		c.Engine.Preferred = v
	}
	if v := os.Getenv("OPOD_REQUIRE_KEYS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Auth.RequireKeys = b
		}
	}
	if v := os.Getenv("OPOD_JOIN_TOKEN"); v != "" {
		c.Auth.JoinToken = v
	}
	if v := os.Getenv("OPOD_ADMIN_TOKEN"); v != "" {
		c.Auth.AdminToken = v
	}
	if v := os.Getenv("OPOD_DEFAULT_MODEL"); v != "" {
		c.Router.DefaultModel = v
	}
	if v := os.Getenv("OPOD_PULL_DEFAULT_MODEL"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.Router.PullDefaultModel = b
		}
	}
	if v := os.Getenv("OPOD_OTLP_ENDPOINT"); v != "" {
		c.Observability.OTLPEndpoint = v
	}
	if v := os.Getenv("OPOD_LATENCY_P95_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Router.LatencyFallbackP95Seconds = n
		}
	}
	if v := os.Getenv("OPOD_PLACEMENT_ALLOWED_FAILS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Router.PlacementAllowedFails = n
		}
	}
	if v := os.Getenv("OPOD_PLACEMENT_COOLDOWN_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Router.PlacementCooldownSeconds = n
		}
	}
	if v := os.Getenv("OPOD_HEARTBEAT_MAX_AGE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Router.HeartbeatMaxAgeSeconds = n
		}
	}
	if v := os.Getenv("OPOD_STICKY_SESSION_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Router.StickySessionTTLSeconds = n
		}
	}
	if v := os.Getenv("OPOD_EXCLUSIVE"); v != "" {
		// ParseBool so "1"/"true"/"TRUE"/"yes"-style values behave like the
		// other boolean env knobs (OPOD_REQUIRE_KEYS), instead of only the
		// exact strings "1"/"true".
		if b, err := strconv.ParseBool(v); err == nil {
			c.Placement.Exclusive = b
		}
	}
	if v := os.Getenv("OPOD_PLACEMENT_RESERVE_PERCENT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n < 100 {
			c.Placement.ReservePercent = n
		}
	}
	if v := os.Getenv("OPOD_PLACEMENT_DRAIN_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Placement.DrainTimeoutSeconds = n
		}
	}
}

// expand replaces ~ at the start of paths with the home directory.
func expand(c *Config) {
	home, _ := os.UserHomeDir()
	expandOne := func(p string) string {
		if strings.HasPrefix(p, "~/") {
			return filepath.Join(home, p[2:])
		}
		return p
	}
	c.DataDir = expandOne(c.DataDir)
	c.Storage.DSN = expandOne(c.Storage.DSN)
	c.Storage.ModelsDir = expandOne(c.Storage.ModelsDir)
	c.CatalogDir = expandOne(c.CatalogDir)
}

func ensureDirs(c *Config) error {
	for _, d := range []string{c.DataDir, c.Storage.ModelsDir} {
		if d == "" {
			continue
		}
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}
