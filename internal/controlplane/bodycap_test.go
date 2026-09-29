package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// /v1 bodies are capped per route (PLAN T15.3): the chat cap (configurable,
// 32 MiB by default), 8 MiB for embeddings, 1 MiB for anything else. One byte
// over answers 413 with an OpenAI-shaped body before any handler runs; one
// under reaches the handler.
func TestV1BodyCapsArePerRouteAnd413(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = false
	cfg.MaxBodyBytes = 4096 // the chat cap is the configurable one
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	postWithin := func(path string, size int, within time.Duration) (int, map[string]any) {
		t.Helper()
		// A body of exactly size bytes that is NOT valid JSON: a body under
		// the cap therefore reaches the handler and is answered 400 at once,
		// which proves the cap did not trip without driving the engine.
		body := "{" + strings.Repeat("x", size-1)
		ctx, cancel := context.WithTimeout(context.Background(), within)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+path, bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	assert413 := func(path string, size int) {
		t.Helper()
		code, out := postWithin(path, size, 30*time.Second)
		if code != http.StatusRequestEntityTooLarge {
			t.Fatalf("%s at %d bytes: %d %v, want 413", path, size, code, out)
		}
		e, _ := out["error"].(map[string]any)
		if e["type"] != "request_too_large" || !strings.Contains(e["message"].(string), "limit of") {
			t.Fatalf("%s: the 413 is not OpenAI-shaped: %v", path, out)
		}
	}
	// Under the cap the request reaches the handler, which refuses the
	// malformed body as 400 — anything but a 413.
	assertNot413 := func(path string, size int) {
		t.Helper()
		code, out := postWithin(path, size, 30*time.Second)
		if code != http.StatusBadRequest {
			t.Fatalf("%s at %d bytes: %d %v, want the handler's 400 (the cap must not trip)", path, size, code, out)
		}
	}
	assert413("/v1/chat/completions", 4097)
	assertNot413("/v1/chat/completions", 4096)
	assert413("/v1/embeddings", 8<<20+1)
	assertNot413("/v1/embeddings", 4097) // over the chat cap, under its own
}
