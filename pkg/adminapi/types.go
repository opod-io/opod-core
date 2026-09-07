// Package adminapi is the typed decoupling contract between an opod leader
// and whatever manages it (the control plane today, anything else tomorrow):
// the JSON shapes served on /admin/v1, /loadz and the mounted auth/policy
// snapshot files. The leader marshals these types itself, so a field that
// changes here changes on the wire; a manager mirrors them and checks the
// mirror against this file (the control plane's TestLeaderTypesInSync).
//
// Rules: additive only within a contract version; json tags are the wire;
// no methods that reach into internal packages (this package imports nothing
// but the standard library and is the seed of the opod-sdk module).
package adminapi

import "time"

// ContractVersion is reported by /admin/v1/version and /admin/v1/capabilities.
const ContractVersion = "v1"

// Route is one method+pattern pair of the frozen /admin/v1 surface
// (chi placeholders exactly as the leader registers them).
type Route struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// Version is GET /admin/v1/version.
type Version struct {
	Version  string `json:"version"`
	Contract string `json:"contract"`
}

// Capabilities is GET /admin/v1/capabilities: the frozen route list plus the
// feature flags a manager may probe instead of sniffing behaviour.
type Capabilities struct {
	Contract string          `json:"contract"`
	Routes   []Route         `json:"routes"`
	Features map[string]bool `json:"features"`
}

// Load is GET /loadz — what the autoscaler reads.
type Load struct {
	PlanRevision    int    `json:"plan_revision"`
	PlanModel       string `json:"plan_model"`
	InFlight        int64  `json:"in_flight"`
	RPM1m           int64  `json:"rpm_1m"`
	Unavailable1m   int64  `json:"unavailable_1m"`
	LastRequestUnix int64  `json:"last_request_unix"`
	TS              int64  `json:"ts"`
}

// UsageData is one usage fact: a request the leader served.
type UsageData struct {
	APIKeyID         string  `json:"api_key_id"`
	UserID           string  `json:"user_id"`
	Model            string  `json:"model"`
	Protocol         string  `json:"protocol"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	LatencyMS        int     `json:"latency_ms"`
	Outcome          string  `json:"outcome"`
	CostUSD          float64 `json:"cost_usd"`
	NodeID           string  `json:"node_id"` // worker that served it ("" = answered locally / never dispatched)
}

// UsageEvent is one row of GET /admin/v1/usage/stream.
type UsageEvent struct {
	ID   int64     `json:"id"`
	Type string    `json:"type"` // "usage"
	TS   time.Time `json:"ts"`
	Data UsageData `json:"data"`
}

// UsageBatch is the page GET /admin/v1/usage/stream returns.
type UsageBatch struct {
	Events []UsageEvent `json:"events"`
	Next   int64        `json:"next"`
	More   bool         `json:"more"`
	Boot   int64        `json:"boot,omitempty"` // leader process start; ids restart with it
}

// LifecycleEvent is one row of GET /admin/v1/events/stream.
type LifecycleEvent struct {
	ID      int64          `json:"id"`
	Type    string         `json:"type"`
	Subject string         `json:"subject"`
	TS      time.Time      `json:"ts"`
	Data    map[string]any `json:"data"`
}

// EventsBatch is the page GET /admin/v1/events/stream returns.
type EventsBatch struct {
	Events []LifecycleEvent `json:"events"`
	Next   int64            `json:"next"`
	More   bool             `json:"more"`
	Boot   int64            `json:"boot,omitempty"`
}

// AuthSnapshot is the auth file a manager mounts (OPOD_AUTH_FILE).
type AuthSnapshot struct {
	Revision    string        `json:"revision"`
	RequireKeys bool          `json:"requireKeys"`
	Keys        []SnapshotKey `json:"keys"`
}

// SnapshotKey is one API key in the auth snapshot (hash, never plaintext).
type SnapshotKey struct {
	ID            string   `json:"id"`
	Name          string   `json:"name"`
	Hash          string   `json:"hash"` // sha256 hex of the plaintext key
	Scope         string   `json:"scope,omitempty"`
	RPMLimit      int      `json:"rpmLimit,omitempty"`
	TPMLimit      int      `json:"tpmLimit,omitempty"`
	AllowedModels []string `json:"allowedModels,omitempty"`
	ExpiresAt     string   `json:"expiresAt,omitempty"` // RFC 3339
}

// PolicySnapshot is the policy file a manager mounts (OPOD_POLICY_FILE).
type PolicySnapshot struct {
	Revision   string          `json:"revision"`
	Routing    PolicyRouting   `json:"routing"`
	Logging    PolicyLogging   `json:"logging"`
	Guardrails []GuardrailRule `json:"guardrails"`
}

// PolicyRouting names the fallback target used after the pre-guardrail chain
// when the leader has no capacity (an OpenAI-compatible base URL).
type PolicyRouting struct {
	FallbackURL   string `json:"fallbackUrl,omitempty"`
	FallbackModel string `json:"fallbackModel,omitempty"`
	FallbackKey   string `json:"fallbackKey,omitempty"`
}

// PolicyLogging switches the leader's access log; nil = unchanged.
type PolicyLogging struct {
	AccessLog *bool `json:"accessLog,omitempty"`
}

// GuardrailRule is one webhook guardrail the leader instantiates.
type GuardrailRule struct {
	ID        string            `json:"id"`
	Phase     string            `json:"phase"` // pre | post | logging_only
	URL       string            `json:"url"`
	AuthKey   string            `json:"authKey,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	FailOpen  bool              `json:"failOpen"`
	TimeoutMs int               `json:"timeoutMs,omitempty"`
}
