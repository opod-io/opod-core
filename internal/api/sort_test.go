package api

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/router"
	"github.com/opod-io/opod/internal/store"
)

func TestMergeBodyAndHeaders_Sort(t *testing.T) {
	// Body wins over header.
	got := mergeBodyAndHeaders(&opodExtras{Sort: "latency"}, http.Header{"X-Opod-Sort": {"price"}})
	if got.Sort != "latency" {
		t.Errorf("body sort should win, got %q", got.Sort)
	}
	// Header fills when body empty.
	got = mergeBodyAndHeaders(nil, http.Header{"X-Opod-Sort": {"price"}})
	if got.Sort != "price" {
		t.Errorf("header sort = %q, want price", got.Sort)
	}
	// Garbage clamps to empty (ignored, not a 400 — forward compat).
	if c := (router.Overrides{Sort: "cheapest"}).Clamp(); c.Sort != "" {
		t.Errorf("unknown sort survived Clamp: %q", c.Sort)
	}
}

// TestOverridesContext_SuffixPrecedence: explicit opod.sort beats the
// :floor/:nitro suffix hint; the suffix applies when nothing explicit
// is present.
func TestOverridesContext_SuffixPrecedence(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	ctx := overridesContext(req, &opodExtras{Sort: "latency"}, nil, "m", "price")
	if got := router.FromContext(ctx).Sort; got != "latency" {
		t.Errorf("explicit sort should beat suffix, got %q", got)
	}

	ctx = overridesContext(req, nil, nil, "m", "price")
	if got := router.FromContext(ctx).Sort; got != "price" {
		t.Errorf("suffix hint should apply when nothing explicit, got %q", got)
	}

	// No sort anywhere → overrides not set at all (zero-allocation path).
	ctx = overridesContext(req, nil, nil, "m", "")
	if router.FromContext(ctx).IsSet() {
		t.Errorf("no overrides expected")
	}
}

// TestModelAllowMiddleware_SortSuffix: an allowlist of ["x"] must
// authorize "x:floor" — the suffix is a routing hint, not a different
// model.
func TestModelAllowMiddleware_SortSuffix(t *testing.T) {
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	key := &store.APIKey{
		ID: "k_test", Hash: "h", Name: "alice", Scope: "user", UserID: "alice",
		AllowedModels: []string{"qwen3-14b"},
	}
	if err := st.APIKeys().Create(context.Background(), *key); err != nil {
		t.Fatal(err)
	}
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("ok"))
	})
	mw := ModelAllowMiddleware(st)(downstream)

	send := func(model string) int {
		body := []byte(`{"model":"` + model + `","messages":[]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req = req.WithContext(auth.WithTestKey(context.Background(), key))
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, req)
		return w.Code
	}
	if code := send("qwen3-14b:floor"); code != http.StatusOK {
		t.Errorf("allowed model + :floor suffix → %d, want 200", code)
	}
	if code := send("qwen3-14b:nitro"); code != http.StatusOK {
		t.Errorf("allowed model + :nitro suffix → %d, want 200", code)
	}
	if code := send("gpt-4o:floor"); code != http.StatusForbidden {
		t.Errorf("disallowed model + suffix → %d, want 403", code)
	}
}
