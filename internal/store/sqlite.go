// Package store provides persistent state for the control plane.
//
// The default backend is SQLite (pure Go via modernc.org/sqlite — no CGO).
// All schemas are managed by inline migrations applied at Open() time.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the persistence abstraction. Implementations must be safe for
// concurrent use.
type Store interface {
	APIKeys() APIKeyStore
	Models() ModelStore
	Nodes() NodeStore
	Placements() PlacementStore
	DesiredPlacements() DesiredPlacementStore
	Shards() ShardStore
	Usage() UsageStore
	EventLog() EventLogStore
	Audit() AuditStore
	Budgets() BudgetStore
	// Cache is the persistent backend for the response cache. The
	// cache package wraps this with its driver-shape API.
	Cache() CacheStore
	// Route is the persisted cross-provider routing chain — the ordered
	// list of model ids Opod walks for model="auto" (and as fallback),
	// editable from the CLI and dashboard.
	Route() RouteStore
	Close() error
}

// RouteStore persists the routing chain: an ordered list of model ids
// (vendor-prefixed like "groq/llama-3.3-70b" or local like "qwen3.6-27b")
// that the router tries top-to-bottom. Stored as a single JSON row.
type RouteStore interface {
	// Get returns the stored chain, or nil if none has been set (the caller
	// then falls back to the computed default).
	Get(ctx context.Context) ([]string, error)
	// Set replaces the entire chain.
	Set(ctx context.Context, chain []string) error
}

// CacheStore is the persistent cache surface. Values are opaque
// bytes; expiry is a unix-epoch second.
type CacheStore interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key, namespace string, value []byte, expiresAt time.Time) error
	Delete(ctx context.Context, key string) error
	DeleteNamespace(ctx context.Context, namespace string) error
	// DeleteAll truncates the cache table — every namespace, every
	// key. Backs the admin endpoint's `?all=1` flush.
	DeleteAll(ctx context.Context) error
	// SweepExpired deletes rows whose expires_at < now. Called by the
	// cache package's reaper.
	SweepExpired(ctx context.Context, now time.Time) (int64, error)
	Count(ctx context.Context) (int, int64, error) // entries, total bytes
}

// ---- types ----

// APIKey represents a Opod API key. Plain key text is never stored; only Hash.
//
// AllowedModels, when non-nil, restricts the key to the listed model ids
// — requests for any other model are refused with HTTP 403
// `model_not_allowed`. A nil slice (the default, and the value for keys
// created before this column existed) means "no restriction". An empty
// slice means "no model is allowed" — useful for hard-disabling a key
// without revoking it. Entries support glob suffix wildcards (e.g.
// `claude-*`, `gpt-*`) so vendor families can be approved in one row.
//
// RPMLimit and TPMLimit are per-minute token-bucket ceilings. 0 means
// unlimited (the default for legacy keys). Enforced by
// api.RateLimitMiddleware via in-memory leaky buckets; resets on
// leader restart.
//
// ExpiresAt, when non-zero, makes the key time-limited: the auth
// middleware refuses requests with HTTP 401 `key_expired` once `now >
// expires_at`. Zero (the default for legacy keys) means "never
// expires". Useful for short-lived per-PR keys and external-contractor
// access.
type APIKey struct {
	ID               string
	Hash             string
	Name             string
	Scope            string // "admin" | "user" | "node"
	UserID           string
	QuotaDailyTokens int64
	RPMLimit         int
	TPMLimit         int
	AllowedModels    []string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	Revoked          bool
}

type APIKeyStore interface {
	Create(ctx context.Context, k APIKey) error
	GetByHash(ctx context.Context, hash string) (*APIKey, error)
	GetByID(ctx context.Context, id string) (*APIKey, error)
	List(ctx context.Context) ([]APIKey, error)
	Revoke(ctx context.Context, id string) error
	// UpdateAllowedModels replaces the allowlist for the given key id.
	// Pass nil for "unrestricted", an empty slice for "deny all", or a
	// list (each entry may include a `*` suffix wildcard).
	UpdateAllowedModels(ctx context.Context, id string, allowed []string) error
	// UpdateRateLimits replaces the RPM/TPM ceilings on the given key.
	// Passing 0 means "unlimited" — restores the legacy behavior. Both
	// fields are set atomically so a partial edit can't accidentally
	// leave one ceiling set and the other clear.
	UpdateRateLimits(ctx context.Context, id string, rpm, tpm int) error
	// UpdateExpiresAt sets the key's expiry. A zero time clears the
	// expiry ("never expires"); a past time effectively expires the
	// key immediately.
	UpdateExpiresAt(ctx context.Context, id string, expiresAt time.Time) error
}

type Model struct {
	ID          string
	CatalogID   string
	Source      string
	Status      string
	SizeBytes   int64
	InstalledAt time.Time
}

type ModelStore interface {
	Upsert(ctx context.Context, m Model) error
	Get(ctx context.Context, id string) (*Model, error)
	List(ctx context.Context) ([]Model, error)
	Delete(ctx context.Context, id string) error
}

// Node represents a worker registered with the control plane.
//
// WorkerToken is a long-lived shared secret used for BOTH directions of
// communication between the leader and this worker. v0.3 stores it in
// plaintext for simplicity — a future revision will replace this with
// per-direction HMAC keys.
//
// BoundKeyID records the API key id that first registered this node.
// Subsequent register/heartbeat calls with a node-scope key must present
// the same key id, so one leaked node token can't impersonate every
// node. Empty ("" — legacy rows) means "not yet bound"; the binding is
// established on the next successful register.
type Node struct {
	ID            string
	Hostname      string
	OS            string
	Arch          string
	RAMGB         int
	Address       string
	WorkerToken   string
	BoundKeyID    string
	HardwareJSON  string
	LastHeartbeat time.Time
	State         string
}

type NodeStore interface {
	Upsert(ctx context.Context, n Node) error
	Get(ctx context.Context, id string) (*Node, error)
	List(ctx context.Context) ([]Node, error)
	Delete(ctx context.Context, id string) error
}

// Placement records that a given node currently hosts a given model.
// The special node id "local" represents the leader's own engine.
type Placement struct {
	NodeID   string
	ModelID  string
	Status   string // ready | loading | error
	LastSeen time.Time
}

type PlacementStore interface {
	Upsert(ctx context.Context, p Placement) error
	GetByModel(ctx context.Context, modelID string) ([]Placement, error)
	GetByNode(ctx context.Context, nodeID string) ([]Placement, error)
	ReplaceForNode(ctx context.Context, nodeID string, ps []Placement) error
	Delete(ctx context.Context, nodeID, modelID string) error
	// SetStatus flips a single placement's status in place (e.g. ready ↔
	// draining during an eviction) without touching last_seen semantics.
	// A draining placement is invisible to GetByModel, so the router
	// stops sending new requests to it.
	SetStatus(ctx context.Context, nodeID, modelID, status string) error
}

// DesiredPlacement is the operator's declared intent that a node should
// keep a model resident in memory. Distinct from Placement (which is
// observational: "this node can serve this model right now").
//
// ModelID is the CATALOG id — surfaces map it to the engine-native name
// (Ollama tag, HF repo) via the models table when talking to an engine.
//
// Priority orders both restore-on-boot (high first) and eviction (low
// first). Pinned placements are never chosen as eviction victims and are
// loaded with Ollama keep_alive=-1 so the engine's idle TTL skips them.
type DesiredPlacement struct {
	NodeID    string    `json:"node_id"`
	ModelID   string    `json:"model_id"`
	Priority  int       `json:"priority"`
	Pinned    bool      `json:"pinned"`
	CreatedAt time.Time `json:"created_at"`
}

type DesiredPlacementStore interface {
	Upsert(ctx context.Context, d DesiredPlacement) error
	Get(ctx context.Context, nodeID, modelID string) (*DesiredPlacement, error)
	// ListByNode returns the node's desired set ordered by priority DESC,
	// created_at ASC — the order Restore loads them in.
	ListByNode(ctx context.Context, nodeID string) ([]DesiredPlacement, error)
	Delete(ctx context.Context, nodeID, modelID string) error
}

// Shard is one piece of a model that has been split across multiple nodes.
// A sharded model has N "rpc" shards (one per node hosting a piece of the
// model) plus exactly one "coordinator" shard (the node running
// llama-server --rpc <list>). The router routes requests to the coordinator
// only; the coordinator talks to the rpc shards internally.
type Shard struct {
	ID         string
	ModelID    string
	Role       string // "coordinator" | "rpc"
	NodeID     string
	Address    string // host:port reachable by the coordinator (rpc) or by the leader (coordinator)
	ProcessID  string // id assigned by the supervisor that launched the process
	Status     string // starting | ready | failed | stopped
	ConfigJSON string
	CreatedAt  time.Time
	LastSeen   time.Time
}

type ShardStore interface {
	Create(ctx context.Context, s Shard) error
	Get(ctx context.Context, id string) (*Shard, error)
	GetByModel(ctx context.Context, modelID string) ([]Shard, error)
	UpdateStatus(ctx context.Context, id, status string) error
	List(ctx context.Context) ([]Shard, error)
	Delete(ctx context.Context, id string) error
	DeleteByModel(ctx context.Context, modelID string) error
}

// Usage records a single completed inference request.
//
// CostUSD is the dollar cost computed from the catalog/vendor pricing
// table at write time (`recordUsage`). 0 means no cost was tracked —
// either the row predates the column, or the model has no pricing
// configured (typical for operator-owned open-weight models). Storing
// the snapshot lets historical totals stay correct even when pricing
// changes later.
type Usage struct {
	ID               int64
	TS               time.Time
	APIKeyID         string
	UserID           string
	Model            string
	Protocol         string
	PromptTokens     int
	CompletionTokens int
	LatencyMS        int
	Outcome          string
	CostUSD          float64
}

// Budget is one rolling spend / token allowance attached to an API key.
// Multiple budgets can apply to a single key — every request must clear
// all of them to be admitted. Two simultaneously-valid configurations:
//
//   - tokens / day  (e.g. 1M tokens/day)
//   - usd / month   (e.g. $100/month)
//
// CurrentValue accumulates as requests run (incremented from the
// recordUsage path); a request is refused when CurrentValue >=
// LimitValue. ResetAt is the unix timestamp at which the window
// rolls — checked lazily on every read so we don't need a cron.
type Budget struct {
	ID           int64
	APIKeyID     string
	Window       string // "day" | "week" | "month"
	LimitUnit    string // "tokens" | "usd"
	LimitValue   float64
	CurrentValue float64
	ResetAt      time.Time
	CreatedAt    time.Time
}

// BudgetStore is the persistence surface for per-key budgets.
type BudgetStore interface {
	Create(ctx context.Context, b Budget) (int64, error)
	ListByKey(ctx context.Context, apiKeyID string) ([]Budget, error)
	Delete(ctx context.Context, id int64) error
	// Increment atomically adds delta to a single budget. Used from
	// recordUsage after the response is known.
	Increment(ctx context.Context, id int64, delta float64) error
	// ResetExpired rolls every budget whose reset_at has passed:
	// current_value = 0, reset_at = next boundary. Called from the
	// middleware so admission decisions always see fresh state.
	ResetExpired(ctx context.Context, apiKeyID string, now time.Time) error
}

type UsageStore interface {
	Record(ctx context.Context, u Usage) error
	SumTokensSince(ctx context.Context, apiKeyID string, since time.Time) (int64, error)
	// LastUsedByModel returns the most recent request timestamp per model
	// id. Models with no usage rows are absent from the map — eviction
	// treats them as least-recently-used. Used by the lifecycle manager's
	// LRU victim ordering.
	LastUsedByModel(ctx context.Context) (map[string]time.Time, error)
	RecentByUser(ctx context.Context, userID string, limit int) ([]Usage, error)
	Recent(ctx context.Context, limit int) ([]Usage, error)
	// After returns up to limit rows with id > afterID in id order — the
	// cursor pull an external collector uses for at-least-once capture.
	// The AUTOINCREMENT id is the cursor: monotonic, never reused.
	After(ctx context.Context, afterID int64, limit int) ([]Usage, error)
	// Breakdown aggregates the usage table by time bucket and the
	// chosen grouping columns. Each non-empty group_by entry adds a
	// SELECT column and a GROUP BY term. Returns the rows + the
	// totals across all rows in the same bucket range.
	Breakdown(ctx context.Context, opts BreakdownOpts) ([]BreakdownRow, BreakdownTotals, error)
}

// BreakdownOpts describes the time-bucketed query.
type BreakdownOpts struct {
	// Bucket selects the time-rollup granularity. Valid: "hour",
	// "day", "month", "total". Defaults to "day" when empty.
	Bucket string
	// Since / Until bound the time range. Zero values mean "open
	// ended" — Since defaults to 30 days ago, Until to now.
	Since time.Time
	Until time.Time
	// GroupBy is an ordered subset of {"user","model","protocol","outcome"}.
	// Empty groups by the time bucket only.
	GroupBy []string
	// Limit caps the number of returned rows. 0 = no limit.
	Limit int
}

// BreakdownRow is one aggregated row.
type BreakdownRow struct {
	Bucket           string  `json:"bucket"`
	User             string  `json:"user,omitempty"`
	Model            string  `json:"model,omitempty"`
	Protocol         string  `json:"protocol,omitempty"`
	Outcome          string  `json:"outcome,omitempty"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Requests         int64   `json:"requests"`
	CostUSD          float64 `json:"cost_usd"`
}

// BreakdownTotals sums everything in the bucket range — useful for the
// dashboard "totals" footer and as a sanity check that group_by didn't
// double-count.
type BreakdownTotals struct {
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	Requests         int64   `json:"requests"`
	CostUSD          float64 `json:"cost_usd"`
}

// AuditEntry records an action taken via the admin API or gateway.
type AuditEntry struct {
	ID       int64
	TS       time.Time
	Actor    string
	Action   string
	Target   string
	Metadata string
}

type AuditStore interface {
	Record(ctx context.Context, e AuditEntry) error
	Recent(ctx context.Context, limit int) ([]AuditEntry, error)
}

// ---- open ----

func OpenSQLite(dsn string) (Store, error) {
	db, err := sql.Open("sqlite", appendPragmas(dsn))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := applySchema(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

type sqliteStore struct {
	db *sql.DB
}

func (s *sqliteStore) APIKeys() APIKeyStore       { return &sqliteAPIKeys{db: s.db} }
func (s *sqliteStore) Models() ModelStore         { return &sqliteModels{db: s.db} }
func (s *sqliteStore) Nodes() NodeStore           { return &sqliteNodes{db: s.db} }
func (s *sqliteStore) Placements() PlacementStore { return &sqlitePlacements{db: s.db} }
func (s *sqliteStore) EventLog() EventLogStore    { return &sqliteEventLog{db: s.db} }
func (s *sqliteStore) DesiredPlacements() DesiredPlacementStore {
	return &sqliteDesiredPlacements{db: s.db}
}
func (s *sqliteStore) Shards() ShardStore   { return &sqliteShards{db: s.db} }
func (s *sqliteStore) Usage() UsageStore    { return &sqliteUsage{db: s.db} }
func (s *sqliteStore) Audit() AuditStore    { return &sqliteAudit{db: s.db} }
func (s *sqliteStore) Budgets() BudgetStore { return &sqliteBudgets{db: s.db} }
func (s *sqliteStore) Cache() CacheStore    { return &sqliteCache{db: s.db} }
func (s *sqliteStore) Route() RouteStore    { return &sqliteRoute{db: s.db} }
func (s *sqliteStore) Close() error         { return s.db.Close() }

type sqliteRoute struct{ db *sql.DB }

// Get returns the persisted chain, or nil when unset so the caller can fall
// back to the computed default.
func (s *sqliteRoute) Get(ctx context.Context) ([]string, error) {
	row := s.db.QueryRowContext(ctx, `SELECT chain_json FROM route_chain WHERE id = 1`)
	var blob string
	if err := row.Scan(&blob); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("scan route: %w", err)
	}
	var chain []string
	if err := json.Unmarshal([]byte(blob), &chain); err != nil {
		return nil, fmt.Errorf("decode route: %w", err)
	}
	return chain, nil
}

// Set replaces the whole chain (singleton row keyed id=1).
func (s *sqliteRoute) Set(ctx context.Context, chain []string) error {
	if chain == nil {
		chain = []string{}
	}
	blob, err := json.Marshal(chain)
	if err != nil {
		return fmt.Errorf("encode route: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO route_chain(id, chain_json, updated_at) VALUES(1, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET chain_json=excluded.chain_json, updated_at=excluded.updated_at`,
		string(blob), time.Now().Unix())
	if err != nil {
		return fmt.Errorf("set route: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
    id                  TEXT PRIMARY KEY,
    hash                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    scope               TEXT NOT NULL,
    user_id             TEXT NOT NULL DEFAULT '',
    quota_daily_tokens  INTEGER NOT NULL DEFAULT 0,
    rpm_limit           INTEGER NOT NULL DEFAULT 0,
    tpm_limit           INTEGER NOT NULL DEFAULT 0,
    allowed_models      TEXT,
    expires_at          INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL,
    revoked             INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_api_keys_hash ON api_keys(hash);
CREATE INDEX IF NOT EXISTS idx_api_keys_expires ON api_keys(expires_at) WHERE expires_at > 0;

CREATE TABLE IF NOT EXISTS models (
    id           TEXT PRIMARY KEY,
    catalog_id   TEXT NOT NULL,
    source       TEXT NOT NULL,
    status       TEXT NOT NULL,
    size_bytes   INTEGER NOT NULL DEFAULT 0,
    installed_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS nodes (
    id             TEXT PRIMARY KEY,
    hostname       TEXT NOT NULL,
    os             TEXT NOT NULL,
    arch           TEXT NOT NULL,
    ram_gb         INTEGER NOT NULL,
    address        TEXT NOT NULL DEFAULT '',
    worker_token   TEXT NOT NULL DEFAULT '',
    bound_key_id   TEXT NOT NULL DEFAULT '',
    hardware_json  TEXT NOT NULL,
    last_heartbeat INTEGER NOT NULL,
    state          TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS model_placements (
    node_id    TEXT NOT NULL,
    model_id   TEXT NOT NULL,
    status     TEXT NOT NULL,
    last_seen  INTEGER NOT NULL,
    PRIMARY KEY (node_id, model_id)
);
CREATE INDEX IF NOT EXISTS idx_placements_model ON model_placements(model_id);

CREATE TABLE IF NOT EXISTS desired_placements (
    node_id    TEXT NOT NULL,
    model_id   TEXT NOT NULL,
    priority   INTEGER NOT NULL DEFAULT 0,
    pinned     INTEGER NOT NULL DEFAULT 0,
    created_at INTEGER NOT NULL,
    PRIMARY KEY (node_id, model_id)
);

CREATE TABLE IF NOT EXISTS shards (
    id          TEXT PRIMARY KEY,
    model_id    TEXT NOT NULL,
    role        TEXT NOT NULL,
    node_id     TEXT NOT NULL,
    address     TEXT NOT NULL,
    process_id  TEXT NOT NULL,
    status      TEXT NOT NULL,
    config_json TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_shards_model ON shards(model_id);

CREATE TABLE IF NOT EXISTS usage (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                INTEGER NOT NULL,
    api_key_id        TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    model             TEXT NOT NULL,
    protocol          TEXT NOT NULL,
    prompt_tokens     INTEGER NOT NULL,
    completion_tokens INTEGER NOT NULL,
    latency_ms        INTEGER NOT NULL,
    outcome           TEXT NOT NULL,
    cost_usd          REAL NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_usage_key_ts ON usage(api_key_id, ts);
CREATE INDEX IF NOT EXISTS idx_usage_user_ts ON usage(user_id, ts);
CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage(ts);
CREATE INDEX IF NOT EXISTS idx_usage_model_ts ON usage(model, ts);

CREATE TABLE IF NOT EXISTS budgets (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    api_key_id     TEXT    NOT NULL,
    window         TEXT    NOT NULL,                -- 'day' | 'week' | 'month'
    limit_unit     TEXT    NOT NULL,                -- 'tokens' | 'usd'
    limit_value    REAL    NOT NULL,
    current_value  REAL    NOT NULL DEFAULT 0,
    reset_at       INTEGER NOT NULL,
    created_at     INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_budgets_key ON budgets(api_key_id);

CREATE TABLE IF NOT EXISTS cache (
    key        TEXT PRIMARY KEY,
    namespace  TEXT NOT NULL DEFAULT '',
    value      BLOB NOT NULL,
    expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_cache_expires_at ON cache(expires_at);
CREATE INDEX IF NOT EXISTS idx_cache_namespace ON cache(namespace);

CREATE TABLE IF NOT EXISTS audit_log (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            INTEGER NOT NULL,
    actor         TEXT NOT NULL,
    action        TEXT NOT NULL,
    target        TEXT NOT NULL,
    metadata_json TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);

CREATE TABLE IF NOT EXISTS route_chain (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    chain_json TEXT NOT NULL DEFAULT '[]',
    updated_at INTEGER NOT NULL
);
`

func applySchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, eventLogSchema); err != nil {
		return fmt.Errorf("event log schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	return runColumnMigrations(ctx, db)
}

// runColumnMigrations brings older databases up to the current column set.
// Each migration is idempotent — checks whether the column exists before
// adding it. SQLite does not support `ADD COLUMN IF NOT EXISTS`, so the
// presence check uses PRAGMA table_info.
//
// Currently empty (cost_micros was added in v0.6 and removed in v0.7 —
// the column will remain in upgraded-from-v0.6 databases as an unused
// nullable field, which is harmless). Add new column migrations here as
// the schema evolves.
func runColumnMigrations(ctx context.Context, db *sql.DB) error {
	type colMigration struct {
		table, column, ddl string
	}
	migrations := []colMigration{
		// v0.8 — per-key model allowlist. NULL preserves the existing
		// "any model" behavior for keys created before this column.
		{table: "api_keys", column: "allowed_models", ddl: `ALTER TABLE api_keys ADD COLUMN allowed_models TEXT`},
		// v0.8 — per-key RPM / TPM ceilings. 0 = unlimited.
		{table: "api_keys", column: "rpm_limit", ddl: `ALTER TABLE api_keys ADD COLUMN rpm_limit INTEGER NOT NULL DEFAULT 0`},
		{table: "api_keys", column: "tpm_limit", ddl: `ALTER TABLE api_keys ADD COLUMN tpm_limit INTEGER NOT NULL DEFAULT 0`},
		// v0.8 — per-call $ cost. 0 default — pre-migration rows have no
		// cost recorded.
		{table: "usage", column: "cost_usd", ddl: `ALTER TABLE usage ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`},
		// v0.8 — per-key expiry. 0 = never expires (legacy default).
		{table: "api_keys", column: "expires_at", ddl: `ALTER TABLE api_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`},
		// v0.9 — first-use node↔key binding. '' = unbound (legacy rows);
		// bound on the node's next successful register.
		{table: "nodes", column: "bound_key_id", ddl: `ALTER TABLE nodes ADD COLUMN bound_key_id TEXT NOT NULL DEFAULT ''`},
	}
	for _, m := range migrations {
		exists, err := columnExists(ctx, db, m.table, m.column)
		if err != nil {
			return fmt.Errorf("check column %s.%s: %w", m.table, m.column, err)
		}
		if exists {
			continue
		}
		if _, err := db.ExecContext(ctx, m.ddl); err != nil {
			return fmt.Errorf("migrate %s.%s: %w", m.table, m.column, err)
		}
	}
	return nil
}

func columnExists(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// appendPragmas safely appends the required pragma settings to a DSN that
// may already contain a query string.
func appendPragmas(dsn string) string {
	// busy_timeout makes a writer wait (up to 5s) for the write lock instead
	// of failing immediately with SQLITE_BUSY. One *sql.DB is shared across
	// heartbeats, usage, budgets, and the cache reaper, so concurrent writes
	// are routine; under WAL readers don't block writers, so this — not
	// SetMaxOpenConns(1), which would also serialize reads — is the fix.
	pragmas := "_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	return dsn + sep + pragmas
}
