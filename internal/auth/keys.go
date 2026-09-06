// Package auth handles API key generation, hashing, validation, and the
// HTTP middleware that gates protected routes.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// keyPrefix is the visible prefix on every Opod-issued API key.
const keyPrefix = "sk-orc-"

// contextKey is an unexported type to avoid context-key collisions.
type contextKey string

const (
	ctxKeyAPIKey contextKey = "opod.apikey"
	ctxKeyScope  contextKey = "opod.scope"
)

// Generate creates a new random API key, returning the plaintext key
// (shown to the user once) and its sha256 hash (stored in the DB).
//
// userID identifies the owner. Pass "" for tokens with no owner concept
// (e.g., node-join tokens, the very first admin key).
func Generate(name, scope, userID string) (plain string, rec store.APIKey, err error) {
	buf := make([]byte, 24)
	if _, err = rand.Read(buf); err != nil {
		return "", store.APIKey{}, fmt.Errorf("rand: %w", err)
	}
	plain = keyPrefix + base64.RawURLEncoding.EncodeToString(buf)
	id := randID()
	rec = store.APIKey{
		ID:        id,
		Hash:      Hash(plain),
		Name:      name,
		Scope:     scope,
		UserID:    userID,
		CreatedAt: time.Now(),
	}
	return plain, rec, nil
}

// Hash returns the sha256 hex digest of a plain API key.
func Hash(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// Middleware returns an http middleware that enforces API key auth.
// If requireKeys is false, requests proceed without auth (dev only).
func Middleware(keys store.APIKeyStore, requireKeys bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !requireKeys {
				// Dev mode: no auth enforced. Inject an admin scope so the
				// scope-gated routes (admin + node register/heartbeat) stay
				// reachable — otherwise ScopeFrom returns "" and every
				// RequireScope/RequireScopeAny check 403s, which makes a
				// keyless cluster unable to register nodes or be administered.
				ctx := context.WithValue(r.Context(), ctxKeyScope, "admin")
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			plain, ok := extractKey(r)
			if !ok {
				writeError(w, http.StatusUnauthorized, "missing_api_key", "Authorization header required")
				return
			}
			key, err := keys.GetByHash(r.Context(), Hash(plain))
			if err != nil {
				writeError(w, http.StatusInternalServerError, "auth_error", err.Error())
				return
			}
			if key == nil || key.Revoked {
				writeError(w, http.StatusUnauthorized, "invalid_api_key", "Invalid or revoked API key")
				return
			}
			if !key.ExpiresAt.IsZero() && time.Now().After(key.ExpiresAt) {
				writeError(w, http.StatusUnauthorized, "key_expired",
					"API key expired at "+key.ExpiresAt.UTC().Format(time.RFC3339))
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyAPIKey, key)
			ctx = context.WithValue(ctx, ctxKeyScope, key.Scope)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireScope returns a middleware that 403s if the request's scope doesn't match.
func RequireScope(want string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ScopeFrom(r.Context()) != want {
				writeError(w, http.StatusForbidden, "forbidden", "Insufficient scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireScopeAny returns a middleware that 403s unless the request scope is
// one of the allowed values.
func RequireScopeAny(scopes ...string) func(http.Handler) http.Handler {
	allow := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		allow[s] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allow[ScopeFrom(r.Context())] {
				writeError(w, http.StatusForbidden, "forbidden", "Insufficient scope")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ScopeFrom returns the scope attached to the request context (or "").
func ScopeFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxKeyScope).(string)
	return v
}

// KeyFrom returns the API key record attached to the request context (or nil).
func KeyFrom(ctx context.Context) *store.APIKey {
	v, _ := ctx.Value(ctxKeyAPIKey).(*store.APIKey)
	return v
}

func extractKey(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if h != "" {
		if strings.HasPrefix(h, "Bearer ") {
			return strings.TrimPrefix(h, "Bearer "), true
		}
		return h, true
	}
	// Anthropic-style header. Header.Get canonicalizes the name, so this
	// single lookup matches `x-api-key`, `X-Api-Key`, and friends.
	if h := r.Header.Get("X-Api-Key"); h != "" {
		return h, true
	}
	// Query parameter for endpoints that browsers can't open with custom
	// headers (notably EventSource for SSE). The query lives only in
	// server memory; it doesn't leak past this process unless an access
	// log is configured to capture URLs. Operators who care can disable
	// query-string logging or front-end the dashboard with a proxy that
	// strips this param.
	if k := r.URL.Query().Get("key"); k != "" {
		return k, true
	}
	return "", false
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body := fmt.Sprintf(`{"error":{"type":"%s","message":%q}}`, code, message)
	_, _ = w.Write([]byte(body))
}

func randID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// extremely unlikely; fall back to a non-random ID
		return fmt.Sprintf("k_%d", time.Now().UnixNano())
	}
	return "k_" + base64.RawURLEncoding.EncodeToString(buf)
}

// ErrNoKey is returned when no API key is present in a context that requires one.
var ErrNoKey = errors.New("no API key in context")

// WithTestKey attaches an APIKey to ctx the way the auth middleware would,
// so other packages can write integration tests that exercise downstream
// middleware (model allowlist, quota) without spinning up a real auth
// round-trip. Production code paths never call this — use Middleware.
func WithTestKey(ctx context.Context, key *store.APIKey) context.Context {
	ctx = context.WithValue(ctx, ctxKeyAPIKey, key)
	if key != nil {
		ctx = context.WithValue(ctx, ctxKeyScope, key.Scope)
	}
	return ctx
}
