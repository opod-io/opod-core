package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// When require_keys is false (dev mode), the middleware must still inject a
// scope so the admin/node scope-gated routes stay reachable. Regression for
// the keyless-cluster lockout where ScopeFrom returned "" and every
// RequireScope check 403'd.
func TestDevModeInjectsAdminScope(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	// Middleware(requireKeys=false) -> RequireScope("admin") -> final.
	h := Middleware(nil, false)(RequireScope("admin")(final))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/admin/v1/nodes", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dev mode admin route: got %d, want 200 (scope not injected)", rec.Code)
	}
}

// The same scope also satisfies the node-or-admin group used by register/heartbeat.
func TestDevModeSatisfiesScopeAny(t *testing.T) {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	h := Middleware(nil, false)(RequireScopeAny("admin", "node")(final))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/register", nil)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dev mode node route: got %d, want 200", rec.Code)
	}
}

// With keys optional, a caller who presents a valid key is that key's caller:
// the request carries the key, so its usage row is attributed (T9.31). A key
// that is unknown, revoked or expired is ignored — nothing is refused in this
// mode — and the dev-mode scope stays.
func TestOptionalKeysStillAttributeAPresentedKey(t *testing.T) {
	var got *store.APIKey
	var scope string
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, scope = KeyFrom(r.Context()), ScopeFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	live := &store.APIKey{ID: "k_live", Scope: "user"}
	for _, c := range []struct {
		name   string
		key    *store.APIKey
		header string
		want   string
	}{
		{"a valid key", live, "Bearer sk-orc-test", "k_live"},
		{"no key", live, "", ""},
		{"an unknown key", live, "Bearer sk-orc-other", ""},
		{"a revoked key", &store.APIKey{ID: "k_rev", Revoked: true}, "Bearer sk-orc-test", ""},
		{"an expired key", &store.APIKey{ID: "k_old", ExpiresAt: time.Now().Add(-time.Hour)}, "Bearer sk-orc-test", ""},
	} {
		got, scope = nil, ""
		h := Middleware(&fakeKeyStore{key: c.key}, false)(final)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		if c.header != "" {
			req.Header.Set("Authorization", c.header)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		id := ""
		if got != nil {
			id = got.ID
		}
		if rec.Code != http.StatusOK || id != c.want || scope != "admin" {
			t.Errorf("%s: status %d, key %q (want %q), scope %q", c.name, rec.Code, id, c.want, scope)
		}
	}
}
