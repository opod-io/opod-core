package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
