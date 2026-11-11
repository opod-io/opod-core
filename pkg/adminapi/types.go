// Package adminapi is the typed decoupling contract between an opod leader
// and whatever manages it (the control plane today, anything else tomorrow):
// the JSON shapes served on /admin/v1, /loadz and the mounted auth/policy
// snapshot files. The leader marshals these types itself, so a field that
// changes here changes on the wire; the leader and every manager import this
// one package, so nothing mirrors it. It lived in the opod-sdk module until
// core took it in (PLAN T9.50); it is the wire, not a supported client API.
//
// Rules: additive only within a contract version; json tags are the wire;
// this package imports nothing but the standard library.
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
	// Engine load signals aggregated over the model's live workers (build
	// item 14; feature "load_signals"): the max KV-cache use, the summed
	// queue, the summed generation rate, the prefix-cache hit rate, how many
	// workers are alive and how many of them reported a sample. PrefixHitPct
	// is hit tokens ÷ query tokens over the samples that state the counters
	// (PrefixHitTokensTotal, below; PLAN T28.7), else the mean of the rates the
	// samples state.
	KVUsedPct    float64 `json:"kv_used_pct"`
	QueueDepth   int64   `json:"queue_depth"`
	TokensPerSec float64 `json:"tokens_per_s"`
	PrefixHitPct float64 `json:"prefix_hit_pct"`
	Workers      int     `json:"workers"`
	Reporting    int     `json:"reporting"`
	// KVBusyWorkers is the KV pressure as a PROPORTIONAL figure: the mean
	// KV-cache use across the reporting workers, times the units a scaler
	// adds — each worker, and each gang ONCE however many parts it has —
	// "how many workers' (or gangs') worth of KV cache is in use". A horizontal scaler
	// computes replicas as ceil(metric ÷ target), which only means anything
	// for a metric that grows with the replica count; KVUsedPct is a maximum
	// over workers and can never ask for more than two. Always on the wire —
	// 0 when no worker reports — because a scaler reads it by path and a
	// missing key is an error that blocks scale-down, not a zero.
	KVBusyWorkers float64 `json:"kv_busy_workers"`
	// PrefixIndex says whether the block scorer has anything to score with
	// (feature "kv_block_events"): how many live workers report cache events
	// and how many blocks the leader holds for them. Absent on a leader
	// without the feature or with nothing reported.
	PrefixIndex *PrefixIndex `json:"prefix_index,omitempty"`
	// Time to the first token a client can use, over the last minute
	// (R15.13; feature "ttft"). 0 means "not measured in the last minute"
	// (nothing streamed), never "instant". Always on the wire, like
	// KVBusyWorkers: a scaler reads it by path and a missing key is an error
	// that blocks scale-down, while a quiet minute's 0 sits below any budget
	// and lets the other triggers decide.
	TTFTP50Ms int64 `json:"ttft_p50_ms"`
	TTFTP95Ms int64 `json:"ttft_p95_ms"`
	// EnginesUnhealthy is how many of the workers counted above are holding a
	// card while their engine is NOT serving — crash-looping or stopped
	// (feature "engine_liveness"). They heartbeat like any other worker, so
	// anything counting workers counts them as capacity; they are not, and a
	// scaler that believes the count scales out too late or not at all.
	// EngineIssue is the worst one's own last word, for a human.
	EnginesUnhealthy int    `json:"engines_unhealthy,omitempty"`
	EngineIssue      string `json:"engine_issue,omitempty"`
	// What the engines themselves say they allocated (PLAN T23.2, feature
	// "kv_readback"): the sum over the samples counted in Reporting — a
	// worker's engine, or a gang's coordinator, which holds the whole gang's
	// pool — of the same fields of nodeapi.EngineLoad. A total sums only the samples that state it, and is
	// absent when none does: never a zero for "unknown". A worker's own
	// figures are on GET /admin/v1/nodes (its `load`).
	KVCapacityTokens *int64 `json:"kv_capacity_tokens,omitempty"`
	KVUsedTokens     *int64 `json:"kv_used_tokens,omitempty"`
	PreemptionsTotal *int64 `json:"preemptions_total,omitempty"`
	// SpecDraftTokensTotal and SpecAcceptedTokensTotal are the engines'
	// speculative-decoding counters (ADR-120), summed over the samples that
	// state them: accepted ÷ draft is the endpoint's acceptance rate. Absent
	// when no engine speculates.
	SpecDraftTokensTotal    *int64 `json:"spec_draft_tokens_total,omitempty"`
	SpecAcceptedTokensTotal *int64 `json:"spec_accepted_tokens_total,omitempty"`
	// SpecAcceptPct is the mean of the engines' own acceptance rates (0–100)
	// over the samples that state one — SGLang, which publishes no counters.
	// Absent when none does.
	SpecAcceptPct  *float64 `json:"spec_accept_pct,omitempty"`
	EvictionsTotal *int64   `json:"evictions_total,omitempty"`
	// The engines' prefix-cache counters (PLAN T28.7), summed like the fields
	// above: their ratio is PrefixHitPct, and two reads of them give the rate
	// over the window between.
	PrefixHitTokensTotal   *int64 `json:"prefix_hit_tokens_total,omitempty"`
	PrefixQueryTokensTotal *int64 `json:"prefix_query_tokens_total,omitempty"`
	// The engines' host-memory KV tiers (PLAN T26.4, feature "kv_tier_load"):
	// the counters summed over the samples that state them, like the fields
	// above, and TierUsedPct the fullest tier's fill (a maximum, as KVUsedPct
	// is). Absent when no sample states it.
	TierHitTokensTotal     *int64   `json:"tier_hit_tokens_total,omitempty"`
	TierQueryTokensTotal   *int64   `json:"tier_query_tokens_total,omitempty"`
	TierLoadBytesTotal     *int64   `json:"tier_load_bytes_total,omitempty"`
	TierStoreBytesTotal    *int64   `json:"tier_store_bytes_total,omitempty"`
	TierUsedPct            *float64 `json:"tier_used_pct,omitempty"`
	TierAllocFailuresTotal *int64   `json:"tier_alloc_failures_total,omitempty"`
	// TierGhost is what a host KV tier WOULD have served, measured from the
	// engines' own cache events (PLAN T26.14, feature "tier_ghost"): the
	// leader remembers every block a worker's engine evicted, and when the
	// engine stores the same block again it had to compute it again — a tier
	// holding at least that many evicted blocks would have handed it back.
	// Absent when no worker publishes cache events.
	TierGhost *TierGhost `json:"tier_ghost,omitempty"`
	// EngineTTFTP50Ms is the median, over the samples that state it, of the
	// engines' own nodeapi.EngineLoad.EngineTTFTP50Ms (PLAN T24.25, feature
	// "engine_ttft"). TTFTP50Ms minus it is what the leader and the network
	// add. Absent when no sample states it.
	EngineTTFTP50Ms *int64 `json:"engine_ttft_p50_ms,omitempty"`
	// AdmissionBound says which bound the leader's admission gate holds the
	// plan's model to (PLAN T23.11, feature "admission_kv_bound"):
	// AdmissionBoundSlots when the engines' slot counts are the limit,
	// AdmissionBoundKV when their KV pools hold fewer requests of the
	// endpoint's context than they have slots, and the gate queues at that
	// smaller number. Absent when the leader does not say: no plan model, no
	// worker can serve, or a worker reports no slot count (admission is then
	// unbounded).
	AdmissionBound string `json:"admission_bound,omitempty"`
	// LoadSamples is, per worker node id, the sample this process's picker
	// ranks that worker on (feature "load_pull", ADR-109): the leader and
	// every door pull GET /v1/load themselves, so each process has its own
	// map, and the leader's does not include the doors'. Absent before any
	// pull answers.
	LoadSamples map[string]LoadSample `json:"load_samples,omitempty"`
	// Sessions counts, over the last minute, the requests that named a
	// conversation and what their pins did (feature "session_routing",
	// ADR-109). Absent while routing.session is off; present with zeros when
	// it is on and no caller sent a session — Requests1m against RPM1m is the
	// share of traffic that names one. Each process counts its own: the
	// leader's does not include the doors'.
	Sessions *SessionLoad `json:"sessions,omitempty"`
}

// SessionLoad is Load.Sessions. A request is counted once its answer is done.
type SessionLoad struct {
	// Requests1m: requests that named a conversation (X-Opod-Session, else
	// the OpenAI user field).
	Requests1m int64 `json:"requests_1m"`
	// PinHits1m: of those, the ones that landed on the worker their pin named.
	PinHits1m int64 `json:"pin_hits_1m"`
	// PinMoves1m: the ones whose pinned worker was saturated, full or gone,
	// so they routed by load and the pin moved. A request with no pin yet (a
	// first turn, or an expired pin) is in neither.
	PinMoves1m int64 `json:"pin_moves_1m"`
	// RouteTokens1m: the ones that echoed a route token that verified.
	RouteTokens1m int64 `json:"route_tokens_1m"`
}

// LoadSample is one worker's entry in Load.LoadSamples.
type LoadSample struct {
	// Source is LoadSourcePull, or LoadSourceHeartbeat for a worker whose
	// core has no /v1/load (it is ranked on its heartbeat's sample).
	Source string `json:"source"`
	// AgeMs is how old the pulled sample is, by the worker's own clock stamp.
	// 0 for a heartbeat source.
	AgeMs int64 `json:"age_ms,omitempty"`
	// Ranked is false once a pulled sample is older than three
	// routing.loadRefreshMs intervals: the worker is then scored on what is in
	// flight alone.
	Ranked bool `json:"ranked"`
	// Why says, for a heartbeat source, why the worker is not pulled.
	Why string `json:"why,omitempty"`
}

// The values of LoadSample.Source.
const (
	LoadSourcePull      = "pull"
	LoadSourceHeartbeat = "heartbeat"
)

// The values of Load.AdmissionBound.
const (
	AdmissionBoundSlots = "slots"
	AdmissionBoundKV    = "kv"
)

// TierGhost counts, since the reporting workers started, the blocks their
// engines computed and the evicted blocks they computed again, by eviction
// depth. Buckets ascend; a returned block is counted once, in the smallest
// bucket whose MaxDepthBlocks covers the evictions on its worker between its
// eviction and its return. A tier of T blocks would have served the returns of
// every bucket whose MaxDepthBlocks ≤ T.
type TierGhost struct {
	BlockSize    int           `json:"block_size"`
	StoredBlocks int64         `json:"stored_blocks"`
	Returns      []GhostBucket `json:"returns"`
}

// GhostBucket is one depth bucket of TierGhost.Returns.
type GhostBucket struct {
	MaxDepthBlocks int64 `json:"max_depth_blocks"`
	Blocks         int64 `json:"blocks"`
}

// UsageData is one usage fact: a request the leader served.
type UsageData struct {
	APIKeyID string `json:"api_key_id"`
	// TTFTMS is the time to the first usable byte of a STREAMED answer
	// (R15.13). 0 on a non-streamed response, where the whole answer arrives
	// at once and latency_ms is the only honest number.
	TTFTMS           int    `json:"ttft_ms,omitempty"`
	UserID           string `json:"user_id"`
	Model            string `json:"model"`
	Protocol         string `json:"protocol"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	LatencyMS        int    `json:"latency_ms"`
	Outcome          string `json:"outcome"`
	// CostUSD is DEPRECATED and always 0: rating and showback are out of this
	// product (ADR-003). It is kept for one additive tag so a client reading it
	// does not break, and will be removed in the next breaking release.
	CostUSD float64 `json:"cost_usd"`
	NodeID  string  `json:"node_id"` // worker that served it ("" = answered locally / never dispatched)
}

// UsageEvent is one row of GET /admin/v1/usage/stream. A leader with feature
// usage_push has no stream (it pushes OTLP records, usagelog.go); these types
// stay for a manager that still pulls a leader older than that.
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
	// NodeMTLS is how the leader treats a worker's client certificate on the
	// join path (register and heartbeat): "" or "off" = certificates are
	// ignored and a join token is the credential · "allow" = a worker that
	// presents a certificate this leader's node CA signed is authenticated by
	// it, and one that does not falls back to its token · "require" = a join
	// or heartbeat without such a certificate is refused.
	//
	// It lives in the AUTH snapshot and not in the environment on purpose: a
	// fleet moves to mTLS one endpoint at a time, and the mode has to be able
	// to go back to "allow" without restarting a leader that is serving
	// (ADR-005's P1 — HMAC and join tokens stay the GA path).
	NodeMTLS string `json:"nodeMtls,omitempty"`
	// NodeCertCA is the PEM (one or more certificates) a leader verifies a
	// worker's client certificate against. The snapshot carries it rather than
	// a file path because it is not a secret — it is the public half — and an
	// operator rotating the CA should not need a pod restart.
	NodeCertCA string `json:"nodeCertCa,omitempty"`
	// RevokedCerts are certificate serial numbers this leader must refuse even
	// though its CA signed them, as lowercase hex without separators. A
	// revocation has to reach the leader through the snapshot it already
	// watches: nothing else is on the request path (ADR-001), and a CRL
	// endpoint would put the manager there.
	RevokedCerts []string `json:"revokedCerts,omitempty"`
	// RevokedKeys are the ids of API keys this leader must refuse even if a
	// row for them is still in Keys: a revocation is a TOMBSTONE, not a
	// removal (ADR-085). An append is something a second writer — anyone with
	// RBAC on the mounted Secret — can deliver correctly while the manager is
	// down, where re-deriving the whole allow list is not. A leader never
	// un-revokes an id it has seen here.
	RevokedKeys []string `json:"revokedKeys,omitempty"`
	// IssuedAt is when the manager wrote this snapshot (RFC 3339). It is what
	// a snapshot's AGE is measured from: the file's mtime changes only when
	// its content does, so a quiet fleet would look infinitely stale. A
	// manager that sets a staleness bound (PolicyAuth.MaxSnapshotAgeSec)
	// re-issues the snapshot on a period shorter than that bound. Empty = the
	// age is unknown, and no bound is enforced against it.
	IssuedAt string `json:"issuedAt,omitempty"`
}

// SnapshotKeyPrefix is how a leader names a key it learned from the auth
// snapshot: SnapshotKeyPrefix + SnapshotKey.ID. Every record the leader writes
// about the key — usage rows, events — carries that name,
// so a manager strips the prefix to find its own key again.
const SnapshotKeyPrefix = "k_cp_"

// SnapshotKey is one endpoint access token in the auth snapshot (hash, never
// plaintext): who is calling, what it is called, until when, and how its
// requests rank when the endpoint is full. It carries no limit: per-key and
// per-user limits, quotas and model access are the owner's gateway's job in
// front of the endpoint (ADR-111, ADR-112).
type SnapshotKey struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Hash      string `json:"hash"` // sha256 hex of the plaintext key
	Scope     string `json:"scope,omitempty"`
	ExpiresAt string `json:"expiresAt,omitempty"` // RFC 3339
	// Class ranks this key's requests when the endpoint is full (ADR-086):
	// "critical", "standard" or "sheddable". Empty = "standard". It ranks; it
	// does not meter. A request may lower its own class with X-Opod-Class,
	// never raise it.
	Class string `json:"class,omitempty"`
	// Team, when set, is the FLOW this key's requests share fairly with the
	// other flows of their class: every key of one team is one flow. Empty =
	// the key is its own flow.
	Team string `json:"team,omitempty"`
}

// PolicySnapshot is the policy file a manager mounts (OPOD_POLICY_FILE).
type PolicySnapshot struct {
	Revision string        `json:"revision"`
	Routing  PolicyRouting `json:"routing"`
	Logging  PolicyLogging `json:"logging"`
	// Auth bounds how the leader treats its own auth snapshot (ADR-085).
	Auth PolicyAuth `json:"auth,omitzero"`
	// Admission says what a saturated endpoint does with the next request
	// (ADR-082). The zero value is the old behaviour: shed at once.
	Admission PolicyAdmission `json:"admission,omitzero"`
	// Wake bounds the leader's own request wake (feature "request_wake").
	// The zero value is the old behaviour: any sleeping worker may be resumed.
	Wake PolicyWake `json:"wake,omitzero"`
}

// PolicyWake is what a manager tells a leader about resuming sleeping workers
// on the request path (feature "wake_no_resume", PLAN T11.31).
type PolicyWake struct {
	// NoResumeInPlace names sleeping workers, by the node id they register
	// with, that the leader must NOT resume for a request: the memory such a
	// worker would wake into is no longer its own — a manager gave its card to
	// another workload while it slept. A request that finds only such sleepers
	// is held by the admission hold (PolicyAdmission.HoldMs) while the manager
	// moves or re-places the worker, and is served when a worker can take it,
	// or answered 503 + Retry-After when the hold ends. A worker the manager
	// itself resumes, or that wakes on its own, serves as usual: the mark stops
	// only the leader's resume. Empty = every sleeper may be resumed in place.
	NoResumeInPlace []string `json:"noResumeInPlace,omitempty"`
}

// PolicyAuth is the endpoint owner's choice between availability and
// revocation under a manager outage (ADR-085 §4). No setting, no judgement.
type PolicyAuth struct {
	// MaxSnapshotAgeSec, when > 0, makes the leader refuse keyed traffic once
	// its auth snapshot's IssuedAt is older than this many seconds, with a
	// reason a human can read. 0 = fail-static: a leader keeps serving from
	// whatever snapshot it holds, for as long as it holds it.
	MaxSnapshotAgeSec int `json:"maxSnapshotAgeSec,omitempty"`
}

// PolicyAdmission is the hold a saturated endpoint applies before it sheds
// (ADR-082). Headroom is a free worker SLOT (ADR-091): the leader dispatches
// to a worker only while its requests in flight are below the slot count the
// worker reports, and holds the rest. With no Classes every request is
// "standard" and the hold is first come, first served; with Classes the
// leader holds one queue per request class, dispatched strictly in class order
// and fairly per flow inside a class (ADR-086).
type PolicyAdmission struct {
	// HoldMs, when > 0, is how long a request waits for a slot before the
	// leader answers 503 + Retry-After. 0 = a request that finds NO worker
	// able to serve is shed at once, as before this field existed, and one
	// that finds every slot busy waits with no deadline of the leader's (the
	// queue the engine kept before ADR-091), bounded by MaxHeld and the
	// client's own timeout.
	HoldMs int `json:"holdMs,omitempty"`
	// MaxHeld caps how many requests may be waiting at once; past it the next
	// one is shed immediately, so a burst cannot pile up goroutines without
	// bound. 0 = the leader's default.
	MaxHeld int `json:"maxHeld,omitempty"`
	// Classes turns on request classes (ADR-086). Empty = every request is
	// "standard" and the hold is first come, first served, exactly as without
	// this field. A class not listed takes HoldMs above and no cap of its own;
	// MaxHeld above then caps every class together, and a full hold sheds the
	// lowest class first.
	Classes []PolicyAdmissionClass `json:"classes,omitempty"`
}

// PolicyAdmissionClass is one request class's own hold (ADR-086).
type PolicyAdmissionClass struct {
	// Name is "critical", "standard" or "sheddable".
	Name string `json:"name"`
	// HoldMs is how long a request of this class waits for capacity before
	// it is shed. 0 = this class is shed at once.
	HoldMs int `json:"holdMs,omitempty"`
	// MaxHeld caps how many of this class wait at once. 0 = no cap of its
	// own; the admission-wide MaxHeld still applies.
	MaxHeld int `json:"maxHeld,omitempty"`
	// MaxShare (ADR-091) is the fraction of the model's worker slots this
	// class may occupy at once, in (0, 1]. A class at its share waits even
	// while slots are free, which keeps that headroom for the classes above
	// it: ordering alone cannot, because a critical arrival would still wait
	// for a whole sheddable answer to finish. The cap is
	// max(1, floor(MaxShare × slots)). 0 = the class's default: critical 1,
	// standard 1, sheddable 0.25.
	MaxShare float64 `json:"maxShare,omitempty"`
}

// DoorRoutes is the routes file a cell door watches (`opod up --role door
// --routes <file>`, feature `door`, ADR-087): which endpoint answers each
// model name. A door is a name lookup, not a router: it forwards a request to
// the route's upstream and chooses nothing else — no worker, no retry, no
// fallback — and it holds no credential of its own, so the upstream's leader
// verifies the caller's key as it always does.
type DoorRoutes struct {
	// Revision names this version of the file; a door publishes the one it
	// serves from (/gatewayz).
	Revision string      `json:"revision"`
	Routes   []DoorRoute `json:"routes"`
}

// DoorRoute is one model name and the endpoint that answers it.
type DoorRoute struct {
	// Alias is the name a client sends as `model`. Unique within the file;
	// a door refuses a file that repeats one, keeping the last good one.
	Alias string `json:"alias"`
	// Upstream is the endpoint's base URL (scheme://host:port, no /v1).
	Upstream string `json:"upstream"`
	// Model, when set and different from Alias, is the model id the upstream
	// serves: the door rewrites the request's `model` to it and changes
	// nothing else. A leader routes by its own model id, so an alias that is
	// not that id reaches it only this way. Empty = the alias IS that id.
	Model string `json:"model,omitempty"`
	// CA is the PEM certificate(s) the door trusts for an https upstream —
	// exactly these, never the system roots. Public material, which is why
	// it may sit in this file. Empty = the system roots (or plain http).
	CA string `json:"ca,omitempty"`
	// Candidates make this an AUTO route (ADR-092 §8, T19.18): the door picks
	// one of these endpoints by Pick and falls back down the list on an
	// unavailable answer. Upstream, Model and CA are then unused. The order is
	// the owner's; nothing in it is a price.
	Candidates []DoorCandidate `json:"candidates,omitempty"`
	// Pick is "first" (the first candidate that can take the request, the
	// default) or "least-loaded" (by each candidate's /loadz).
	Pick string `json:"pick,omitempty"`
	// MaxHops is how many fallbacks one request may take (default 1, at most
	// len(Candidates)-1).
	MaxHops int `json:"maxHops,omitempty"`
}

// DoorCandidate is one endpoint an auto route may send a request to.
type DoorCandidate struct {
	// Alias is the candidate endpoint's own alias, named in the response's
	// X-Opod-Endpoint header.
	Alias    string `json:"alias"`
	Upstream string `json:"upstream"`
	Model    string `json:"model,omitempty"`
	CA       string `json:"ca,omitempty"`
	// Tools says the candidate's model can call tools: a request that carries
	// `tools` skips a candidate without it.
	Tools bool `json:"tools,omitempty"`
}

// PrefixIndex is the state of a leader's prefix-cache block index.
type PrefixIndex struct {
	WorkersReporting int `json:"workers_reporting"`
	Blocks           int `json:"blocks"`
	// CPUBlocks counts the blocks held in the workers' host-memory tiers
	// (feature "kv_cpu_tier_hits"); 0 when no worker reports one.
	CPUBlocks int `json:"cpu_blocks,omitempty"`
}

// The PolicyRouting.PrefixBlockTier values.
const (
	PrefixBlockTierGPU      = "gpu"
	PrefixBlockTierCPULower = "cpu-lower"
)

// PolicySession is routing.session (ADR-109).
type PolicySession struct {
	// TTLSec is how long a session stays pinned to the worker that last
	// served it, in seconds. 0 = StickyTTLSeconds.
	TTLSec int `json:"ttlSec,omitempty"`
	// SignedRoute: every answer to a request with a session carries
	// X-Opod-Session-Route, a token naming the worker that served it, signed
	// with the key every front door of the endpoint shares. A request that
	// echoes it is routed to that worker by any door — the pin survives a
	// conversation alternating doors. A bad, expired or stale token is a miss,
	// never an error.
	SignedRoute bool `json:"signedRoute,omitempty"`
	// TurnPriority: among one flow's held requests, a later turn of an active
	// session leaves first, by min(turns in 120 s, 4) + seconds waited / 2 —
	// so a first turn that waited 8 s outranks any later turn. Classes and the
	// fairness between flows are untouched.
	TurnPriority bool `json:"turnPriority,omitempty"`
}

// PolicyRouting names the fallback target used when the leader has no
// capacity (an OpenAI-compatible base URL).
type PolicyRouting struct {
	FallbackURL   string `json:"fallbackUrl,omitempty"`
	FallbackModel string `json:"fallbackModel,omitempty"`
	FallbackKey   string `json:"fallbackKey,omitempty"`
	// Load-aware worker choice (feature "routing_load_aware"): the leader's
	// picker scores a worker as in-flight + queue depth + KVWeight × (KV cache
	// used / 100), from the workers' own heartbeat samples — a full cache
	// counts as KVWeight extra requests; 0 = in-flight only (the old order).
	// A worker at or above KVSaturationPct is placed behind every worker
	// with headroom and receives new requests only when none has any
	// (0 = off). PrefixAffinity pins a prompt prefix to the worker that last
	// served it (its prefix cache), a bounded table with the sticky TTL.
	KVWeight        float64 `json:"kvWeight,omitempty"`
	KVSaturationPct int     `json:"kvSaturationPct,omitempty"`
	PrefixAffinity  bool    `json:"prefixAffinity,omitempty"`
	// PrefixBlockWeight (feature "kv_block_events") scores a worker by the
	// prefix-cache blocks it ACTUALLY holds: each leading block of the request
	// that a worker's engine reported stored takes PrefixBlockWeight off that
	// worker's load score, so a warm cache can outweigh a little load and
	// never a lot of it. 0 = off, and then — or for a worker whose engine
	// publishes no cache events — PrefixAffinity's pin is what decides.
	PrefixBlockWeight float64 `json:"prefixBlockWeight,omitempty"`
	// SpeedRank is reserved and no leader implements it. A rank by measured
	// decode speed was tried twice on a cluster — by expected time, then as a
	// tie-break only — and gained nothing at any load, so it was not shipped.
	// A policy that sets it still parses; the field changes nothing.
	SpeedRank bool `json:"speedRank,omitempty"`
	// CacheIsolation (feature "cache_salt") says whose requests may share a
	// prefix cache: "team" (every key of one team shares, and teams never
	// do — a key with no team is its own team), "key" (each key alone) or
	// "none" (every caller shares). The leader puts a `cache_salt` on each
	// request to an engine that keys its cache by one (vLLM, SGLang,
	// TensorRT-LLM): an HMAC of the caller's team or key id, never the id
	// itself, and its prefix pins carry the same salt. An engine with no salt
	// shares its cache whatever this says. Empty = "team".
	CacheIsolation string `json:"cacheIsolation,omitempty"`
	// StickyTTLSeconds (feature "sticky_ttl_policy") is how long a session or
	// a prompt prefix stays pinned to the worker that last served it, in
	// seconds. It applies live: a pin placed after the policy lands expires by
	// it, and a pin already placed keeps the expiry it was stamped with.
	// 0 = the leader's own start value (OPOD_STICKY_SESSION_TTL_SECONDS or
	// router.sticky_session_ttl_seconds; with neither, session pins are off
	// and prefix pins expire after 10 minutes).
	StickyTTLSeconds int `json:"stickyTtlSeconds,omitempty"`
	// LoadRefreshMs (feature "load_pull", ADR-109) is how often each router
	// process — the leader and every front door — pulls each worker's engine
	// load to rank on (GET /v1/load on the worker). A sample older than three
	// times this leaves the rank, and the worker is scored on what is in
	// flight alone. 0 = 500 ms; below 100 is read as 100.
	LoadRefreshMs int `json:"loadRefreshMs,omitempty"`
	// Session (feature "session_routing", ADR-109, PLAN T28.3) is how a
	// conversation keeps its worker. A request names its conversation with the
	// X-Opod-Session header, else the OpenAI `user` field; one with neither is
	// pinned per key, as before. nil = sessions pin for StickyTTLSeconds, no
	// signed route, no turn priority.
	Session *PolicySession `json:"session,omitempty"`
	// PrefixBlockTier (feature "kv_cpu_tier_hits") is which tiers a held
	// block is credited from under PrefixBlockWeight: PrefixBlockTierGPU (the
	// card only, the default) or PrefixBlockTierCPULower (also a block the
	// worker's host-memory tier holds, credited at a fraction of a card block:
	// it is one copy away). Empty = PrefixBlockTierGPU.
	PrefixBlockTier string `json:"prefixBlockTier,omitempty"`
	// Revisions splits traffic between plan revisions (feature
	// "routing_weights", ROADMAP R15.17): each worker registers the plan
	// revision it was started for, and the picker chooses a revision group by
	// weight before it chooses a worker inside it. Empty = no split, which is
	// every endpoint that is not mid-canary.
	//
	// A weight is a share, not a percentage: the leader normalises whatever it
	// is given, so [{1, 9}, {2, 1}] is 10 % on revision 2 and so is
	// [{1, 90}, {2, 10}]. A group with no live worker is skipped and its share
	// goes to the others — a canary whose only pod is restarting must not
	// black-hole its share of the traffic.
	Revisions []RevisionWeight `json:"revisions,omitempty"`
}

// PolicyRouting.CacheIsolation values. The empty string is CacheIsolationTeam.
const (
	CacheIsolationNone = "none"
	CacheIsolationTeam = "team"
	CacheIsolationKey  = "key"
)

// RevisionWeight is one plan revision's share of an endpoint's traffic
// (R15.17). Revision matches what a worker registers as its plan revision.
type RevisionWeight struct {
	Revision int `json:"revision"`
	Weight   int `json:"weight"`
}

// PolicyLogging switches the leader's access log; nil = unchanged.
type PolicyLogging struct {
	AccessLog *bool `json:"accessLog,omitempty"`
}
