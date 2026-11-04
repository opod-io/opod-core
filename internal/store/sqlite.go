// Package store provides persistent state for the control plane.
//
// The default backend is SQLite (pure Go via modernc.org/sqlite — no CGO).
// All schemas are managed by inline migrations applied at Open() time.
package store

import (
	"context"
	"database/sql"
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
	EventLog() EventLogStore
	Audit() AuditStore
	// Cache is the persistent backend for the response cache. The
	// cache package wraps this with its driver-shape API.
	Cache() CacheStore
	Close() error
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
// A key is an IDENTITY: who is calling, with which scope, until when. The
// per-key policy that used to ride on it — a model allowlist, RPM/TPM
// ceilings, a daily token quota — left core on 2026-09-28 (ADR-077 §5 in the
// control plane's decisions): per-caller limits belong to the application
// layer in front of an endpoint, not to the runtime. The columns stay in the
// table with their defaults until a schema step drops them.
//
// ExpiresAt, when non-zero, makes the key time-limited: the auth
// middleware refuses requests with HTTP 401 `key_expired` once `now >
// expires_at`. Zero (the default for legacy keys) means "never
// expires". Useful for short-lived per-PR keys and external-contractor
// access.
type APIKey struct {
	ID        string
	Hash      string
	Name      string
	Scope     string // "admin" | "user" | "node"
	UserID    string
	CreatedAt time.Time
	ExpiresAt time.Time
	Revoked   bool
}

type APIKeyStore interface {
	Create(ctx context.Context, k APIKey) error
	GetByHash(ctx context.Context, hash string) (*APIKey, error)
	GetByID(ctx context.Context, id string) (*APIKey, error)
	List(ctx context.Context) ([]APIKey, error)
	Revoke(ctx context.Context, id string) error
	// UpdateExpiresAt sets the key's expiry. A zero time clears the
	// expiry ("never expires"); a past time effectively expires the
	// key immediately.
	UpdateExpiresAt(ctx context.Context, id string, expiresAt time.Time) error
	// Update rewrites a key's hash, name, scope and expiry under its id —
	// how a manager's auth snapshot rotates a key it keeps the id of.
	Update(ctx context.Context, k APIKey) error
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
	// BootID names the worker PROCESS behind this row (minted once per
	// agent start, R10.1). A register or heartbeat carrying a different one
	// is a new incarnation under the same node id: whatever the leader
	// recorded as running on the old one died with it.
	BootID string
	// EngineSilentSince is when this worker's heartbeats stopped carrying its
	// engine's report (loaded_models null: the engine did not answer the
	// worker); zero while they carry one. The worker is alive — it heartbeats
	// — but nothing says what it serves any more; see Node.WhyNoNewWork.
	EngineSilentSince time.Time
}

// Node states the leader writes. "lost" is derived from heartbeat age and
// never stored.
const (
	NodeStateJoining  = "joining"
	NodeStateReady    = "ready"
	NodeStateDraining = "draining"
)

// Draining reports whether an operator took the node out of rotation: it
// keeps its in-flight work and receives nothing new — no request, no shard
// part — until it is undrained.
func (n Node) Draining() bool { return n.State == NodeStateDraining }

type NodeStore interface {
	Upsert(ctx context.Context, n Node) error
	Get(ctx context.Context, id string) (*Node, error)
	List(ctx context.Context) ([]Node, error)
	Delete(ctx context.Context, id string) error
	// SetState writes the state column alone; false = no such node.
	SetState(ctx context.Context, id, state string) (bool, error)
	// Heartbeat records a worker's ping: last_heartbeat, the boot id when one
	// is presented, and joining → ready. It never writes any other state, so
	// a drain set between a heartbeat's read and its write is not lost.
	Heartbeat(ctx context.Context, id string, at time.Time, bootID string) error
	// NoteEngineReport records whether a heartbeat carried the engine's
	// report (Node.EngineSilentSince). Nothing else is written.
	NoteEngineReport(ctx context.Context, id string, reported bool, at time.Time) error
}

// Placement records that a given node currently hosts a given model.
// The special node id "local" represents the leader's own engine.
type Placement struct {
	NodeID   string
	ModelID  string
	Status   string // ready | loading | error | sleeping | draining (PlacementDraining) | released (PlacementReleased)
	LastSeen time.Time
	// Cold: the worker answers for the model but it is not in memory now — an
	// engine that keeps installed weights and loads them on the first request
	// said so (the heartbeat's resident_models). A cold row routes like any
	// other; it holds no memory. False for every engine that cannot tell.
	Cold bool
}

type PlacementStore interface {
	Upsert(ctx context.Context, p Placement) error
	GetByModel(ctx context.Context, modelID string) ([]Placement, error)
	GetByNode(ctx context.Context, nodeID string) ([]Placement, error)
	// All returns every placement row, whatever its status: one read for a
	// caller that judges the whole fleet at once (readiness) instead of one
	// GetByNode per node.
	All(ctx context.Context) ([]Placement, error)
	ReplaceForNode(ctx context.Context, nodeID string, ps []Placement) error
	Delete(ctx context.Context, nodeID, modelID string) error
	// SetStatus flips a single placement's status in place (e.g. ready ↔
	// draining during an eviction) without touching last_seen semantics.
	// A draining placement is invisible to GetByModel, so the router
	// stops sending new requests to it.
	SetStatus(ctx context.Context, nodeID, modelID, status string) error
	// ResetStatus rewrites every placement whose status is from to to, and
	// says how many it changed.
	ResetStatus(ctx context.Context, from, to string) (int64, error)
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

// DefaultGangID is the gang a part belongs to when nothing named one. It is
// what every shard row written before gangs existed means, and what a bare
// `opod shard create <model>` still produces — so a model with one gang reads
// and behaves exactly as it did.
const DefaultGangID = "g0"

// Shard is one piece of a model that has been split across multiple nodes.
//
// A model is split into one or more GANGS. A gang has N "rpc" shards (one per
// node hosting a piece of the model) plus exactly one "coordinator" shard (the
// node running llama-server --rpc <list>). The router routes a request to one
// gang's coordinator; that coordinator talks only to its own gang's rpc shards.
//
// Several gangs of one model are independent copies of the same weights: each
// serves whole requests by itself, so they are throughput, and the router picks
// among their coordinators the way it picks among workers. Parts NEVER span
// gangs — a coordinator that dialled another gang's rpc shard would split one
// request across two copies of the same layers.
type Shard struct {
	ID      string
	ModelID string
	// GangID names which gang of ModelID this part belongs to. Always set on a
	// stored row; Create fills the default when a caller leaves it empty.
	GangID     string
	Role       string // "coordinator" | "rpc"
	NodeID     string
	Address    string // host:port reachable by the coordinator (rpc) or by the leader (coordinator)
	ProcessID  string // id assigned by the supervisor that launched the process
	Status     string // starting | ready | failed | stopped
	ConfigJSON string
	CreatedAt  time.Time
	LastSeen   time.Time
}

// Gang is the gang this part belongs to, defaulted for a value built in memory
// that never named one.
func (s Shard) Gang() string {
	if s.GangID == "" {
		return DefaultGangID
	}
	return s.GangID
}

// SameIncarnation reports whether o is the same incarnation of the part as s:
// the same id, node, address, process and creation time (whole seconds, as
// stored). Status and last_seen change on a live row and say nothing about
// which incarnation it is. It is the rule ShardStore.DeleteRows matches by.
func (s Shard) SameIncarnation(o Shard) bool {
	return s.ID == o.ID && s.NodeID == o.NodeID && s.Address == o.Address &&
		s.ProcessID == o.ProcessID && s.CreatedAt.Unix() == o.CreatedAt.Unix()
}

type ShardStore interface {
	Create(ctx context.Context, s Shard) error
	Get(ctx context.Context, id string) (*Shard, error)
	// GetByModel returns every part of every gang of the model. Callers that
	// reason about one gang — routing, liveness, teardown — want GetByGang or
	// GangsOf instead: a set of parts spanning gangs answers no question about
	// whether any single gang can serve.
	GetByModel(ctx context.Context, modelID string) ([]Shard, error)
	// GetByGang returns one gang's parts. "" means DefaultGangID.
	GetByGang(ctx context.Context, modelID, gangID string) ([]Shard, error)
	// GangsOf lists the model's gang ids, oldest gang first.
	GangsOf(ctx context.Context, modelID string) ([]string, error)
	UpdateStatus(ctx context.Context, id, status string) error
	List(ctx context.Context) ([]Shard, error)
	Delete(ctx context.Context, id string) error
	DeleteByModel(ctx context.Context, modelID string) error
	// DeleteByGang removes one gang, leaving the model's other gangs alone.
	DeleteByGang(ctx context.Context, modelID, gangID string) error
	// DeleteRows removes exactly the rows given, each only while the stored
	// row is still the one that was read — same id, node, address, process
	// and creation time — and returns the ones it removed. A gang formed
	// again under the same id after the rows were read reuses their ids, so
	// deleting by id or by gang would take the NEW gang with it.
	DeleteRows(ctx context.Context, rows []Shard) ([]Shard, error)
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
	// Trim deletes every row older than the newest keep rows.
	Trim(ctx context.Context, keep int) (int64, error)
}

// ---- open ----

func OpenSQLite(dsn string) (Store, error) {
	db, err := sql.Open("sqlite", appendPragmas(dsn))
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if dsn == ":memory:" || strings.Contains(dsn, "mode=memory") {
		db.SetMaxOpenConns(1) // an in-memory database lives in one connection
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
func (s *sqliteStore) Shards() ShardStore { return &sqliteShards{db: s.db} }
func (s *sqliteStore) Audit() AuditStore  { return &sqliteAudit{db: s.db} }
func (s *sqliteStore) Cache() CacheStore  { return &sqliteCache{db: s.db} }
func (s *sqliteStore) Close() error       { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS api_keys (
    id                  TEXT PRIMARY KEY,
    hash                TEXT NOT NULL UNIQUE,
    name                TEXT NOT NULL,
    scope               TEXT NOT NULL,
    user_id             TEXT NOT NULL DEFAULT '',
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
    cold       INTEGER NOT NULL DEFAULT 0,
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
    gang_id     TEXT NOT NULL DEFAULT 'g0',
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
CREATE INDEX IF NOT EXISTS idx_shards_gang ON shards(model_id, gang_id);



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

`

func applySchema(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, eventLogSchema); err != nil {
		return fmt.Errorf("event log schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return err
	}
	if err := runColumnMigrations(ctx, db); err != nil {
		return err
	}
	if err := runColumnDrops(ctx, db); err != nil {
		return err
	}
	return runTableDrops(ctx, db)
}

// droppedTables are tables a release removed. The usage table left with
// feature usage_push (T32.34): a finished request is a record in the leader's
// bounded in-memory queue (internal/usage) until a receiver takes it, so a
// table left behind would only hold rows nothing reads.
var droppedTables = []string{"usage"}

// runTableDrops removes the tables in droppedTables. DROP … IF EXISTS is
// idempotent, and the table's indexes go with it.
func runTableDrops(ctx context.Context, db *sql.DB) error {
	for _, t := range droppedTables {
		if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS `+t); err != nil {
			return fmt.Errorf("drop table %s: %w", t, err)
		}
	}
	return nil
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
		// v0.8 — per-key expiry. 0 = never expires (legacy default).
		{table: "api_keys", column: "expires_at", ddl: `ALTER TABLE api_keys ADD COLUMN expires_at INTEGER NOT NULL DEFAULT 0`},
		// v0.9 — first-use node↔key binding. '' = unbound (legacy rows);
		// bound on the node's next successful register.
		{table: "nodes", column: "bound_key_id", ddl: `ALTER TABLE nodes ADD COLUMN bound_key_id TEXT NOT NULL DEFAULT ''`},
		// R10.1 — the worker incarnation behind the row.
		{table: "nodes", column: "boot_id", ddl: `ALTER TABLE nodes ADD COLUMN boot_id TEXT NOT NULL DEFAULT ''`},
		// Installed but not in memory, as a worker's engine reported it. 0 =
		// resident, which is what every row written before the column meant.
		// Since when a worker's heartbeats carry no engine report. 0 = they do.
		{table: "nodes", column: "engine_silent_since", ddl: `ALTER TABLE nodes ADD COLUMN engine_silent_since INTEGER NOT NULL DEFAULT 0`},
		{table: "model_placements", column: "cold", ddl: `ALTER TABLE model_placements ADD COLUMN cold INTEGER NOT NULL DEFAULT 0`},
		// Which gang of the model this part belongs to. The DEFAULT fills every
		// existing row with the default gang, which is what a model's only gang
		// was before gangs had names — so a row always carries its gang and no
		// reader has to know two spellings for one of them.
		{table: "shards", column: "gang_id", ddl: `ALTER TABLE shards ADD COLUMN gang_id TEXT NOT NULL DEFAULT 'g0'`},
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

// droppedColumns are columns a release removed. Each is dropped from a
// database that still carries it, once; a fresh database never has it. The
// api_keys four carried a key's policy — the per-key rate limits, daily
// token quota and model allowlist — which left the leader on 2026-09-28: a
// key is an identity, and per-caller limits belong to the application layer
// in front of the endpoint. A value left in one of them would be a limit an
// operator believes is enforced and is not.
var droppedColumns = []struct{ table, column string }{
	{"api_keys", "quota_daily_tokens"},
	{"api_keys", "rpm_limit"},
	{"api_keys", "tpm_limit"},
	{"api_keys", "allowed_models"},
}

// runColumnDrops removes the columns in droppedColumns where they still exist.
// Idempotent like the adds: the presence check is the same PRAGMA.
func runColumnDrops(ctx context.Context, db *sql.DB) error {
	for _, d := range droppedColumns {
		exists, err := columnExists(ctx, db, d.table, d.column)
		if err != nil {
			return fmt.Errorf("check column %s.%s: %w", d.table, d.column, err)
		}
		if !exists {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE `+d.table+` DROP COLUMN `+d.column); err != nil {
			return fmt.Errorf("drop column %s.%s: %w", d.table, d.column, err)
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
