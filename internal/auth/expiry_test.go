package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// fakeKeyStore stubs out the APIKeyStore interface so the middleware
// can be tested without spinning up SQLite. Only GetByHash is used by
// the auth path.
type fakeKeyStore struct{ key *store.APIKey }

func (f *fakeKeyStore) Create(context.Context, store.APIKey) error { return nil }
func (f *fakeKeyStore) GetByHash(_ context.Context, h string) (*store.APIKey, error) {
	if f.key != nil && Hash("sk-orc-test") == h {
		return f.key, nil
	}
	return nil, nil
}
func (f *fakeKeyStore) GetByID(context.Context, string) (*store.APIKey, error)      { return f.key, nil }
func (f *fakeKeyStore) List(context.Context) ([]store.APIKey, error)                { return nil, nil }
func (f *fakeKeyStore) Revoke(context.Context, string) error                        { return nil }
func (f *fakeKeyStore) UpdateAllowedModels(context.Context, string, []string) error { return nil }
func (f *fakeKeyStore) UpdateRateLimits(context.Context, string, int, int) error    { return nil }
func (f *fakeKeyStore) UpdateExpiresAt(context.Context, string, time.Time) error    { return nil }

func TestMiddleware_ExpiredKeyReturns401KeyExpired(t *testing.T) {
	expired := &store.APIKey{
		ID:        "k_test",
		Hash:      Hash("sk-orc-test"),
		Scope:     "user",
		UserID:    "alice",
		ExpiresAt: time.Now().Add(-time.Hour), // expired an hour ago
	}
	store := &fakeKeyStore{key: expired}
	mw := Middleware(store, true)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("downstream should not be reached for expired key")
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-orc-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
	if !strings.Contains(w.Body.String(), "key_expired") {
		t.Errorf("body should mention key_expired, got %q", w.Body.String())
	}
}

func TestMiddleware_FutureExpiryAllowsRequest(t *testing.T) {
	fresh := &store.APIKey{
		ID:        "k_test",
		Hash:      Hash("sk-orc-test"),
		Scope:     "user",
		UserID:    "alice",
		ExpiresAt: time.Now().Add(time.Hour),
	}
	mw := Middleware(&fakeKeyStore{key: fresh}, true)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-orc-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !called {
		t.Fatalf("got %d / called=%v", w.Code, called)
	}
}

func TestMiddleware_NeverExpiresAllowsRequest(t *testing.T) {
	never := &store.APIKey{
		ID:     "k_test",
		Hash:   Hash("sk-orc-test"),
		Scope:  "user",
		UserID: "alice",
		// ExpiresAt left zero = never expires.
	}
	mw := Middleware(&fakeKeyStore{key: never}, true)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer sk-orc-test")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK || !called {
		t.Fatalf("got %d / called=%v", w.Code, called)
	}
}
