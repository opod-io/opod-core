package control

// Leader boot steps `opod up` runs against the store before serving: the
// first admin key, the configured join/admin tokens seeded as key rows, and
// the default model made resident. Each returns what happened; the CLI prints
// it (P13-9: cmd/opod parses flags, calls, prints).

import (
	"context"
	"fmt"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// BootstrapAdminKey mints the first admin key when the store has no keys at
// all. Returns the plaintext (the caller shows it once and may save it) or
// "" when keys already exist.
func BootstrapAdminKey(ctx context.Context, st store.Store) (string, error) {
	keys, err := st.APIKeys().List(ctx)
	if err != nil {
		return "", fmt.Errorf("list api keys: %w", err)
	}
	if len(keys) > 0 {
		return "", nil
	}
	plain, rec, err := auth.Generate("initial-admin", "admin", "admin")
	if err != nil {
		return "", fmt.Errorf("generate: %w", err)
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		return "", fmt.Errorf("persist: %w", err)
	}
	return plain, nil
}

// SeedOutcome says what a seed step did, in words the CLI can print.
type SeedOutcome struct {
	Seeded bool   // a new key row was written
	Hint   string // a nudge for the operator when nothing could be seeded ("" = quiet)
}

// SeedJoinToken makes the CONFIGURED node-join token valid on this leader by
// ensuring a matching node-scoped key row exists. The store keeps only the
// key's hash, so a known token seeds deterministically: a leader with a fresh
// or wiped store still accepts workers holding cfg.Auth.JoinToken, which is
// what lets worker joins survive a leader rebuild. Idempotent (the row is
// keyed by the token's hash). With no token configured it only reports
// whether any live node key exists.
func SeedJoinToken(ctx context.Context, st store.Store, cfg *config.Config) (SeedOutcome, error) {
	token := trim(cfg.Auth.JoinToken)
	if token == "" {
		keys, err := st.APIKeys().List(ctx)
		if err != nil {
			return SeedOutcome{}, nil
		}
		for _, k := range keys {
			if k.Scope == "node" && !k.Revoked {
				return SeedOutcome{}, nil
			}
		}
		return SeedOutcome{Hint: "no node-join token yet — run `opod token create --node` (or set OPOD_JOIN_TOKEN) before `opod join` on a worker"}, nil
	}
	hash := auth.Hash(token)
	if existing, err := st.APIKeys().GetByHash(ctx, hash); err == nil && existing != nil && !existing.Revoked {
		return SeedOutcome{}, nil
	}
	// Deterministic id from the hash: the same token always maps to the same
	// key id, so the leader's first-use node binding stays stable across restarts.
	rec := store.APIKey{
		ID:        "k_seed_" + hash[:12],
		Hash:      hash,
		Name:      "fleet-join (seeded)",
		Scope:     "node",
		CreatedAt: time.Now(),
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		return SeedOutcome{}, fmt.Errorf("seed node-join token: %w", err)
	}
	return SeedOutcome{Seeded: true}, nil
}

// SeedAdminToken mirrors SeedJoinToken for the configured admin token: a
// leader provisioned by an external manager accepts that manager's token on
// /admin/v1 even with a fresh store. Idempotent; no-op when unset.
func SeedAdminToken(ctx context.Context, st store.Store, cfg *config.Config) (SeedOutcome, error) {
	token := trim(cfg.Auth.AdminToken)
	if token == "" {
		return SeedOutcome{}, nil
	}
	hash := auth.Hash(token)
	if existing, err := st.APIKeys().GetByHash(ctx, hash); err == nil && existing != nil && !existing.Revoked {
		return SeedOutcome{}, nil
	}
	rec := store.APIKey{
		ID:        "k_seedadm_" + hash[:12],
		Hash:      hash,
		Name:      "manager-admin (seeded)",
		Scope:     "admin",
		CreatedAt: time.Now(),
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		return SeedOutcome{}, fmt.Errorf("seed admin token: %w", err)
	}
	return SeedOutcome{Seeded: true}, nil
}

// DefaultModelOutcome says what EnsureDefaultModel did.
type DefaultModelOutcome struct {
	EngineName string
	Pulled     bool // weights were pulled now (false = already resident)
}

// EnsureDefaultModel records cfg.Router.DefaultModel in the store and pulls
// it when the engine does not have it yet. onProgress receives the engine's
// pull progress (may be nil).
func EnsureDefaultModel(ctx context.Context, cfg *config.Config, cat []models.Entry, st store.Store, eng engines.Engine, onProgress func(status string, completed, total int64)) (DefaultModelOutcome, error) {
	entry := models.FindByID(cat, cfg.Router.DefaultModel)
	if entry == nil {
		return DefaultModelOutcome{}, fmt.Errorf("default model %q not found in catalog", cfg.Router.DefaultModel)
	}
	engineModelName := entry.Source.OllamaName
	if engineModelName == "" {
		engineModelName = entry.ID
	}
	record := func() {
		_ = st.Models().Upsert(ctx, store.Model{
			ID: entry.ID, CatalogID: entry.ID,
			Source: "ollama:" + engineModelName, Status: "ready",
			SizeBytes: entry.SizeBytes, InstalledAt: time.Now(),
		})
	}
	existing, _ := eng.List(ctx)
	for _, m := range existing {
		if m == engineModelName {
			record()
			return DefaultModelOutcome{EngineName: engineModelName}, nil
		}
	}
	if onProgress == nil {
		onProgress = func(string, int64, int64) {}
	}
	if err := eng.Pull(ctx, engineModelName, onProgress); err != nil {
		return DefaultModelOutcome{EngineName: engineModelName}, fmt.Errorf("pull: %w", err)
	}
	record()
	return DefaultModelOutcome{EngineName: engineModelName, Pulled: true}, nil
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
