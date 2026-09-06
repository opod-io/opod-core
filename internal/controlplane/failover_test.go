package controlplane

import (
	"context"
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

func TestFailoverWriter_RetryableSwallowed(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := newFailoverWriter(rec, true) // another candidate remains
	fw.WriteHeader(http.StatusTooManyRequests)
	_, _ = fw.Write([]byte("rate limited"))

	if !fw.retry {
		t.Fatal("429 with canRetry should set retry")
	}
	if fw.committed {
		t.Fatal("retryable attempt must not commit to the client")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("swallowed attempt leaked %q to the client", rec.Body.String())
	}
}

func TestFailoverWriter_SuccessCommits(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := newFailoverWriter(rec, true)
	fw.Header().Set("X-Test", "1")
	fw.WriteHeader(http.StatusOK)
	_, _ = fw.Write([]byte("hello"))

	if fw.retry {
		t.Fatal("200 must not retry")
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "hello" {
		t.Fatalf("commit failed: code=%d body=%q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Test") != "1" {
		t.Fatal("buffered headers were not flushed on commit")
	}
}

func TestFailoverWriter_LastCandidateCommitsEvenOn429(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := newFailoverWriter(rec, false) // last candidate
	fw.WriteHeader(http.StatusTooManyRequests)
	_, _ = fw.Write([]byte("final 429"))

	if fw.retry {
		t.Fatal("last candidate must not retry")
	}
	if rec.Code != http.StatusTooManyRequests || rec.Body.String() != "final 429" {
		t.Fatalf("final error not surfaced: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestFailoverWriter_NonRetryableCommits(t *testing.T) {
	rec := httptest.NewRecorder()
	fw := newFailoverWriter(rec, true) // could retry, but 400 isn't retryable
	fw.WriteHeader(http.StatusBadRequest)
	_, _ = fw.Write([]byte("bad request"))

	if fw.retry {
		t.Fatal("400 must not retry — it's the client's error")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("400 not surfaced: code=%d", rec.Code)
	}
}

// TestAutoWalkFailsOverAcrossProviders drives a model="auto" request through
// the real server: the chain's first provider 429s and the second succeeds,
// so the client transparently gets the second provider's response.
func TestAutoWalkFailsOverAcrossProviders(t *testing.T) {
	up1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"rate_limited"}`)
	}))
	defer up1.Close()
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"served-by-up2","choices":[]}`)
	}))
	defer up2.Close()

	// Configure two registry providers pointing at the upstreams.
	t.Setenv("DEEPSEEK_API_KEY", "k1")
	t.Setenv("DEEPSEEK_BASE_URL", up1.URL)
	t.Setenv("CEREBRAS_API_KEY", "k2")
	t.Setenv("CEREBRAS_BASE_URL", up2.URL)

	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = true

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	// Chain: deepseek (429) → cerebras (200).
	if err := st.Route().Set(ctx, []string{"deepseek/m", "cerebras/m"}); err != nil {
		t.Fatalf("set route: %v", err)
	}

	plain, rec, err := auth.Generate("user-test", "user", "")
	if err != nil {
		t.Fatalf("auth.Generate: %v", err)
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		t.Fatalf("create key: %v", err)
	}

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"auto","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (should have failed over to up2); body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "served-by-up2") {
		t.Fatalf("body = %s, want the second provider's response", body)
	}
}
