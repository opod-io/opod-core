package controlplane

// Auth as a watched file (§13 item 2, v2): in managed mode the executor
// mounts the endpoint's auth snapshot at /etc/opod-auth/auth.json (a Secret
// the kubelet syncs in place) and the leader watches it — no restart, no
// manager call on the request path. The snapshot carries:
//
//   - requireKeys: whether the gateway demands an API key (flips at runtime);
//   - keys: the endpoint's API keys as HASHES with per-key limits, model
//     allowlist and expiry — the leader never sees a plaintext key.
//
// The store stays the rebuildable cache: every key from the file is upserted
// into api_keys under its own id, limits and allowlist are kept in sync, and
// a snapshot-managed key that disappears from the file is revoked. Keys the
// operator minted locally (`opod token create`) are never touched.
//
// No file → no-op: standalone `opod up` keeps its config/CLI behaviour.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

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
}

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
	path := os.Getenv("OPOD_AUTH_FILE")
	if path == "" {
		path = defaultAuthPath
	}
	// Unlike the plan file, the auth file may appear AFTER boot (the first key
	// is minted later), so an absent file is not a reason to stop watching —
	// a stat every 10 s is free. OPOD_AUTH_FILE=off disables the watcher.
	if os.Getenv("OPOD_AUTH_FILE") == "off" {
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
	want := map[string]bool{}
	created, updated, revoked := 0, 0, 0
	for _, sk := range doc.Keys {
		if sk.ID == "" || sk.Hash == "" {
			continue
		}
		id := sk.ID
		if !strings.HasPrefix(id, SnapshotKeyPrefix) {
			id = SnapshotKeyPrefix + id
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
			rec := store.APIKey{ID: id, Hash: sk.Hash, Name: sk.Name, Scope: scope, RPMLimit: sk.RPMLimit, TPMLimit: sk.TPMLimit,
				AllowedModels: sk.AllowedModels, CreatedAt: time.Now(), ExpiresAt: exp}
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
		if cur.RPMLimit != sk.RPMLimit || cur.TPMLimit != sk.TPMLimit {
			if err := keys.UpdateRateLimits(ctx, id, sk.RPMLimit, sk.TPMLimit); err == nil {
				updated++
			}
		}
		if !sameList(cur.AllowedModels, sk.AllowedModels) {
			if err := keys.UpdateAllowedModels(ctx, id, sk.AllowedModels); err == nil {
				updated++
			}
		}
	}
	for _, k := range existing {
		if strings.HasPrefix(k.ID, SnapshotKeyPrefix) && !k.Revoked && !want[k.ID] {
			if err := keys.Revoke(ctx, k.ID); err == nil {
				revoked++
			}
		}
	}
	changed := !s.authf.present || s.authf.revision != doc.Revision || s.authf.requireKeys.Load() != doc.RequireKeys
	s.authf.present = true
	s.authf.revision = doc.Revision
	s.authf.keys = len(doc.Keys)
	s.authf.requireKeys.Store(doc.RequireKeys)
	if changed || created+updated+revoked > 0 {
		s.log.Info("auth snapshot applied", "revision", doc.Revision, "requireKeys", doc.RequireKeys, "keys", len(doc.Keys),
			"created", created, "updated", updated, "revoked", revoked)
		s.logEvent("auth.updated", doc.Revision, map[string]any{"requireKeys": doc.RequireKeys, "keys": len(doc.Keys),
			"created": created, "updated": updated, "revoked": revoked})
	}
	return nil
}

func sameList(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
