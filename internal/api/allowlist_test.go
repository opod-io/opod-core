package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

func TestModelAllowed(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		model   string
		want    bool
	}{
		{"nil → no restriction", nil, "anything-goes", true},
		{"empty → deny all", []string{}, "qwen3-14b", false},
		{"empty → deny all (empty model too)", []string{}, "", false},
		{"literal hit", []string{"qwen3-14b", "gpt-4o"}, "qwen3-14b", true},
		{"literal miss", []string{"qwen3-14b"}, "qwen3-coder-7b", false},
		{"wildcard suffix hit", []string{"claude-*"}, "claude-3-5-sonnet", true},
		{"wildcard suffix prefix-only hit", []string{"gpt-*"}, "gpt-4o-mini", true},
		{"wildcard miss", []string{"claude-*"}, "gpt-4o", false},
		{"bare star = any model", []string{"*"}, "anything", true},
		{"mixed list — literal wins", []string{"qwen3-14b", "claude-*"}, "qwen3-14b", true},
		{"mixed list — wildcard wins", []string{"qwen3-14b", "claude-*"}, "claude-3-haiku", true},
		{"mixed list — both miss", []string{"qwen3-14b", "claude-*"}, "gpt-4o", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ModelAllowed(c.allowed, c.model); got != c.want {
				t.Fatalf("ModelAllowed(%v, %q) = %v, want %v", c.allowed, c.model, got, c.want)
			}
		})
	}
}

// TestModelAllowMiddleware_403 wires the middleware against a real
// sqlite store + a stub key with an allowlist, and verifies that a
// disallowed model returns 403 with the documented error shape and
// that the audit row is recorded.
func TestModelAllowMiddleware_403(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	key := &store.APIKey{
		ID:            "k_test",
		Hash:          "h",
		Name:          "alice",
		Scope:         "user",
		UserID:        "alice",
		AllowedModels: []string{"qwen3-14b", "claude-*"},
	}
	if err := st.APIKeys().Create(ctx, *key); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Downstream handler echoes "ok" — only invoked if the middleware
	// passes the request through.
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Confirm the body is still readable downstream.
		b, _ := io.ReadAll(r.Body)
		if !bytes.Contains(b, []byte(`"model"`)) {
			t.Errorf("downstream got empty body: %q", b)
		}
		_, _ = w.Write([]byte("ok"))
	})
	mw := ModelAllowMiddleware(st)(downstream)

	makeReq := func(model string) *http.Request {
		body := []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		// Inject the key into the request context the way auth.Middleware would.
		req = req.WithContext(auth.WithTestKey(ctx, key))
		return req
	}

	t.Run("allowed literal passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, makeReq("qwen3-14b"))
		if w.Code != http.StatusOK {
			t.Fatalf("got %d body=%q, want 200", w.Code, w.Body.String())
		}
	})
	t.Run("allowed wildcard passes", func(t *testing.T) {
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, makeReq("claude-3-5-sonnet"))
		if w.Code != http.StatusOK {
			t.Fatalf("got %d body=%q, want 200", w.Code, w.Body.String())
		}
	})
	t.Run("disallowed gets 403 with structured error", func(t *testing.T) {
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, makeReq("gpt-4o"))
		if w.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403", w.Code)
		}
		var body struct {
			Error struct {
				Type      string   `json:"type"`
				Requested string   `json:"requested"`
				Allowed   []string `json:"allowed_models"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode 403 body: %v", err)
		}
		if body.Error.Type != "model_not_allowed" {
			t.Errorf("error.type=%q want model_not_allowed", body.Error.Type)
		}
		if body.Error.Requested != "gpt-4o" {
			t.Errorf("error.requested=%q want gpt-4o", body.Error.Requested)
		}
		if len(body.Error.Allowed) != 2 {
			t.Errorf("error.allowed_models=%v want [qwen3-14b claude-*]", body.Error.Allowed)
		}

		// Audit row should have been recorded.
		entries, _ := st.Audit().Recent(ctx, 10)
		found := false
		for _, e := range entries {
			if e.Action == "model_not_allowed" && e.Target == "gpt-4o" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected audit row for model_not_allowed/gpt-4o; got %d entries", len(entries))
		}
	})
}
