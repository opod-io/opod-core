package leader

// Auth as a watched file (§13 item 2, v2): in managed mode the executor
// mounts the endpoint's auth snapshot at /etc/opod-auth/auth.json (a Secret
// the kubelet syncs in place) and the leader watches it — no restart, no
// manager call on the request path. The snapshot carries:
//
//   - requireKeys: whether the gateway demands an API key (flips at runtime);
//   - keys: the endpoint's API keys as HASHES with per-key limits, model
//     allowlist and expiry — the leader never sees a plaintext key;
//   - revokedKeys: TOMBSTONES (ADR-085) — key ids this leader refuses even
//     if a row for them is still in keys. An append is something a second
//     writer with RBAC on the Secret can deliver while the manager is down;
//   - issuedAt: when the manager wrote the snapshot, which is what its AGE
//     is measured from. The policy snapshot's auth.maxSnapshotAgeSec, when
//     set, refuses keyed traffic past that age (staleAuthGate).
//
// The store stays the rebuildable cache: every key from the file is upserted
// into api_keys under its own id, limits and allowlist are kept in sync, and
// a snapshot-managed key that disappears from the file is revoked. Keys the
// operator minted locally (`opod token create`) are never touched.
//
// No file → no-op: standalone `opod up` keeps its config/CLI behaviour.

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

const defaultAuthPath = "/etc/opod-auth/auth.json"

// SnapshotKeyPrefix marks api_keys rows owned by the auth snapshot; only
// those are revoked when they leave the file.
const SnapshotKeyPrefix = "k_cp_"

type authFileState struct {
	mu          sync.Mutex
	present     bool
	revision    string
	requireKeys atomic.Bool
	keys        int
	revoked     int // tombstones in the last snapshot
	// issuedAt is the snapshot's IssuedAt as unix nanoseconds, 0 = unknown.
	// Atomic because the gateway reads it on every keyed request
	// (staleAuthGate) and must never wait on a snapshot reload.
	issuedAt atomic.Int64
	// The node-mTLS policy (R9.6): the mode, the CA the handshake verifies a
	// worker's certificate against, and the serials this leader must refuse
	// even though that CA signed them. Held behind one pointer swapped as a
	// whole (atomic.Pointer) because the join path reads it on every heartbeat
	// of every worker and must never block on a snapshot reload.
	mtls atomic.Pointer[nodeMTLSPolicy]
}

// nodeMTLSPolicy is the snapshot's mTLS half, as the join path reads it.
type nodeMTLSPolicy struct {
	Mode    string // auth.MTLSOff | MTLSAllow | MTLSRequire
	CAPEM   string
	Revoked map[string]bool
	pool    *x509.CertPool // parsed once, on load — not per handshake
}

// nodeMTLS is the policy in force, never nil.
func (s *Server) nodeMTLS() *nodeMTLSPolicy {
	if p := s.authf.mtls.Load(); p != nil {
		return p
	}
	return &nodeMTLSPolicy{Mode: auth.MTLSOff}
}

// ClientCAs is what the TLS handshake verifies a worker certificate against,
// or nil when there is nothing to verify with — in which case no certificate
// can ever be Present and "require" refuses every join, loudly, rather than
// letting one through unchecked.
func (p *nodeMTLSPolicy) ClientCAs() *x509.CertPool { return p.pool }

// AuthSnapshot / SnapshotKey are the shared wire types (opod-sdk/adminapi).
type AuthSnapshot = adminapi.AuthSnapshot
type SnapshotKey = adminapi.SnapshotKey

// requireKeys is the dynamic gate the auth middleware consults: config says
// yes, or a mounted snapshot says yes.
func (s *Server) requireKeys() bool {
	return s.cfg.Auth.RequireKeys || s.authf.requireKeys.Load()
}

// StartAuthWatcher polls the auth file and syncs it into the key store.
func (s *Server) StartAuthWatcher(ctx context.Context) {
	path := s.cfg.Env.AuthFile
	if path == "" {
		path = defaultAuthPath
	}
	// Unlike the plan file, the auth file may appear AFTER boot (the first key
	// is minted later), so an absent file is not a reason to stop watching —
	// a stat every 10 s is free. OPOD_AUTH_FILE=off disables the watcher.
	if s.cfg.Env.AuthFile == "off" {
		return
	}
	var lastMod time.Time
	load := func() {
		st, err := os.Stat(path)
		if err != nil || !st.ModTime().After(lastMod) {
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var doc AuthSnapshot
		if err := json.Unmarshal(raw, &doc); err != nil {
			s.log.Warn("auth file unreadable — keeping last good snapshot", "path", path, "err", err)
			return
		}
		lastMod = st.ModTime()
		if err := s.applyAuthSnapshot(ctx, &doc); err != nil {
			s.log.Warn("auth snapshot not applied", "err", err)
		}
	}
	load()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				load()
			}
		}
	}()
}

// applyAuthSnapshot reconciles api_keys with the snapshot. Idempotent.
func (s *Server) applyAuthSnapshot(ctx context.Context, doc *AuthSnapshot) error {
	s.authf.mu.Lock()
	defer s.authf.mu.Unlock()
	keys := s.store.APIKeys()
	existing, err := keys.List(ctx)
	if err != nil {
		return err
	}
	byID := map[string]store.APIKey{}
	for _, k := range existing {
		byID[k.ID] = k
	}
	// Tombstones first, so a key that is both listed and revoked is never
	// created live for the moment between the two loops.
	tomb := map[string]bool{}
	for _, id := range doc.RevokedKeys {
		if id = strings.TrimSpace(id); id != "" {
			tomb[snapshotKeyID(id)] = true
		}
	}
	want := map[string]bool{}
	created, updated, revoked := 0, 0, 0
	for _, sk := range doc.Keys {
		if sk.ID == "" || sk.Hash == "" {
			continue
		}
		id := snapshotKeyID(sk.ID)
		if tomb[id] {
			continue // revoked below, whatever its row says
		}
		want[id] = true
		scope := sk.Scope
		if scope == "" {
			scope = "user"
		}
		var exp time.Time
		if sk.ExpiresAt != "" {
			exp, _ = time.Parse(time.RFC3339, sk.ExpiresAt)
		}
		cur, ok := byID[id]
		if !ok {
			// A managed key's rpmLimit, tpmLimit, quotaDailyTokens and
			// allowedModels in the snapshot are read by nothing since the per-key
			// policy left core (ADR-077 §5, 2026-09-28); a key is an identity.
			rec := store.APIKey{ID: id, Hash: sk.Hash, Name: sk.Name, Scope: scope, CreatedAt: time.Now(), ExpiresAt: exp}
			if err := keys.Create(ctx, rec); err != nil {
				s.log.Warn("auth snapshot: create key", "id", id, "err", err)
				continue
			}
			created++
			continue
		}
		if cur.Revoked {
			// A revoked row cannot come back; the manager mints a new id instead.
			continue
		}

	}
	for _, k := range existing {
		if strings.HasPrefix(k.ID, SnapshotKeyPrefix) && !k.Revoked && !want[k.ID] && !tomb[k.ID] {
			if err := keys.Revoke(ctx, k.ID); err == nil {
				revoked++
			}
		}
	}
	revoked += s.applyKeyTombstones(ctx, keys, tomb, byID)
	mtlsChanged := s.applyNodeMTLS(doc)
	// IssuedAt is deliberately NOT part of "changed": a manager that sets a
	// staleness bound re-issues the snapshot on a period, and each re-issue
	// would otherwise log a line and stream an auth.updated event that says
	// nothing moved.
	changed := !s.authf.present || s.authf.revision != doc.Revision || s.authf.requireKeys.Load() != doc.RequireKeys ||
		s.authf.revoked != len(tomb) || mtlsChanged
	s.authf.present = true
	s.authf.revision = doc.Revision
	s.authf.keys = len(doc.Keys)
	s.authf.revoked = len(tomb)
	s.authf.requireKeys.Store(doc.RequireKeys)
	s.authf.issuedAt.Store(s.parseIssuedAt(doc.IssuedAt))
	if changed || created+updated+revoked > 0 {
		s.log.Info("auth snapshot applied", "revision", doc.Revision, "requireKeys", doc.RequireKeys, "keys", len(doc.Keys),
			"created", created, "updated", updated, "revoked", revoked, "revokedKeys", len(tomb),
			"nodeMtls", s.nodeMTLS().Mode, "revokedCerts", len(s.nodeMTLS().Revoked))
		s.logEvent("auth.updated", doc.Revision, map[string]any{"requireKeys": doc.RequireKeys, "keys": len(doc.Keys),
			"created": created, "updated": updated, "revoked": revoked, "revokedKeys": len(tomb)})
	}
	return nil
}

// snapshotKeyID is the api_keys row id a snapshot key id maps to. Only ids
// under SnapshotKeyPrefix are the snapshot's to create or revoke, so a
// tombstone can never reach a key the operator minted locally.
func snapshotKeyID(id string) string {
	if strings.HasPrefix(id, SnapshotKeyPrefix) {
		return id
	}
	return SnapshotKeyPrefix + id
}

// tombstoneHash is the hash stored on a row created only to hold a
// tombstone. The column is UNIQUE and NOT NULL, and no sha256 hex digest
// contains a colon, so this never matches a presented key.
func tombstoneHash(id string) string { return "tombstone:" + id }

// applyKeyTombstones revokes every tombstoned id and reports how many rows it
// newly revoked. Called with authf.mu held.
//
// Durability is the store's: a revoked row is never revived by a later
// snapshot (applyAuthSnapshot skips it), so a tombstone seen ONCE outlives the
// snapshot that carried it — which is what makes a break-glass revoke,
// written to the Secret while the manager is down, survive the manager's next
// push of a snapshot that never heard of it. A tombstone for an id this leader
// has no row for yet gets a revoked row of its own: without one, a later
// snapshot listing the key would create it live.
func (s *Server) applyKeyTombstones(ctx context.Context, keys store.APIKeyStore, tomb map[string]bool, byID map[string]store.APIKey) int {
	n := 0
	for id := range tomb {
		cur, ok := byID[id]
		if ok && cur.Revoked {
			continue
		}
		if !ok {
			rec := store.APIKey{ID: id, Hash: tombstoneHash(id), Name: "revoked", Scope: "user", CreatedAt: time.Now()}
			if err := keys.Create(ctx, rec); err != nil {
				s.log.Warn("auth snapshot: tombstone row", "id", id, "err", err)
				continue
			}
		}
		if err := keys.Revoke(ctx, id); err != nil {
			s.log.Warn("auth snapshot: revoke tombstoned key", "id", id, "err", err)
			continue
		}
		n++
	}
	return n
}

// parseIssuedAt turns the snapshot's IssuedAt into unix nanoseconds, 0 when
// it is absent or unreadable. An unreadable one is said once per snapshot and
// then treated as absent: the age is unknown, and no bound is enforced on an
// unknown age (the field's documented meaning).
func (s *Server) parseIssuedAt(v string) int64 {
	if v = strings.TrimSpace(v); v == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		s.log.Warn("auth snapshot: issuedAt is not RFC 3339 — its age is unknown and no staleness bound applies", "issuedAt", v)
		return 0
	}
	return t.UnixNano()
}

// applyNodeMTLS installs the snapshot's mTLS half and reports whether anything
// about it moved. Called with authf.mu held.
//
// A CA that does not parse turns the mode OFF and says so, rather than leaving a
// leader in "require" with nothing to verify against: that state refuses every
// worker in the endpoint, and the reason would be a silent parse failure in a
// file nobody is looking at.
func (s *Server) applyNodeMTLS(doc *AuthSnapshot) bool {
	mode := auth.ParseMTLSMode(doc.NodeMTLS)
	pem := strings.TrimSpace(doc.NodeCertCA)
	var pool *x509.CertPool
	if pem != "" {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(pem)) {
			s.log.Warn("auth snapshot: nodeCertCa holds no certificate — node mTLS stays off", "mode", mode)
			pool, pem, mode = nil, "", auth.MTLSOff
		}
	}
	if mode != auth.MTLSOff && pool == nil {
		s.log.Warn("auth snapshot: nodeMtls asked for but no nodeCertCa — node mTLS stays off", "asked", mode)
		mode = auth.MTLSOff
	}
	next := &nodeMTLSPolicy{Mode: mode, CAPEM: pem, Revoked: auth.RevokedSet(doc.RevokedCerts), pool: pool}
	prev := s.authf.mtls.Load()
	s.authf.mtls.Store(next)
	if prev == nil {
		return mode != auth.MTLSOff || len(next.Revoked) > 0
	}
	return prev.Mode != next.Mode || prev.CAPEM != next.CAPEM || len(prev.Revoked) != len(next.Revoked)
}
