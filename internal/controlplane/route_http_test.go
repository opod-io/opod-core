package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// TestRouteHTTP exercises the /admin/v1/route endpoints end-to-end: default
// on first GET, PUT cleans (trim/blank/dedupe) and persists, and DELETE
// resets to the computed default.
func TestRouteHTTP(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = true

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()

	plain, rec, err := auth.Generate("admin-test", "admin", "")
	if err != nil {
		t.Fatalf("auth.Generate: %v", err)
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		t.Fatalf("APIKeys().Create: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	call := func(method, body string) (int, routeResponse) {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req, _ := http.NewRequest(method, ts.URL+"/admin/v1/route", rdr)
		req.Header.Set("Authorization", "Bearer "+plain)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s /route: %v", method, err)
		}
		defer resp.Body.Close()
		var out routeResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// 1) First GET → computed default (no providers configured → local only).
	code, got := call("GET", "")
	if code != http.StatusOK || !got.IsDefault {
		t.Fatalf("GET default: code=%d is_default=%v", code, got.IsDefault)
	}

	// 2) PUT with blanks + a duplicate → stored cleaned, in order.
	code, got = call("PUT", `{"chain":["groq/llama"," ","deepseek/chat","groq/llama","qwen3.6-27b"]}`)
	if code != http.StatusOK {
		t.Fatalf("PUT: code=%d", code)
	}
	want := []string{"groq/llama", "deepseek/chat", "qwen3.6-27b"}
	if got.IsDefault || !equalStrings(got.Chain, want) {
		t.Fatalf("PUT cleaned = %v (is_default=%v), want %v", got.Chain, got.IsDefault, want)
	}

	// 3) GET → the stored chain, not the default.
	code, got = call("GET", "")
	if code != http.StatusOK || got.IsDefault || !equalStrings(got.Chain, want) {
		t.Fatalf("GET stored = %v (is_default=%v)", got.Chain, got.IsDefault)
	}

	// 4) DELETE → back to default.
	code, got = call("DELETE", "")
	if code != http.StatusOK || !got.IsDefault {
		t.Fatalf("DELETE reset: code=%d is_default=%v", code, got.IsDefault)
	}
}

func equalStrings(a, b []string) bool {
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
