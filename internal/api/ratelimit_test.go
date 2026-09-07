package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

// TestBucket_BurstThenRateLimited — capacity is the per-minute limit;
// a burst above it is denied even when the bucket is freshly minted.
func TestBucket_BurstThenRateLimited(t *testing.T) {
	b := NewBucket(60) // 60/min → 1 token per second refill
	// Consume the entire capacity quickly.
	for i := 0; i < 60; i++ {
		if ok, _ := b.Take(1); !ok {
			t.Fatalf("Take #%d failed; bucket should still have tokens", i+1)
		}
	}
	if ok, retry := b.Take(1); ok {
		t.Fatal("61st request should be rate-limited")
	} else if retry < time.Second {
		t.Errorf("retry-after should be at least 1s (refill rate is 1/sec), got %v", retry)
	}
}

// TestBucket_RefillsLinearly — after waiting, the bucket recovers and
// the next Take succeeds.
func TestBucket_RefillsLinearly(t *testing.T) {
	b := NewBucket(60) // 1 token/sec
	// Drain.
	for i := 0; i < 60; i++ {
		b.Take(1)
	}
	if ok, _ := b.Take(1); ok {
		t.Fatal("bucket should be empty")
	}
	time.Sleep(1100 * time.Millisecond)
	if ok, _ := b.Take(1); !ok {
		t.Fatal("after 1.1s refill, one Take(1) should succeed")
	}
}

// TestBucket_RefundCapsAtCapacity — over-refunding (e.g. from
// reconciliation when actual < estimate) doesn't grow the bucket
// beyond its declared capacity.
func TestBucket_RefundCapsAtCapacity(t *testing.T) {
	b := NewBucket(100)
	b.Take(50) // tokens = 50
	b.Refund(200)
	// Drain to inspect the cap.
	took := 0
	for b != nil {
		if ok, _ := b.Take(1); !ok {
			break
		}
		took++
		if took > 1000 {
			t.Fatal("infinite Take loop — Refund overshot the cap")
		}
	}
	if took != 100 {
		t.Errorf("after over-refund + drain, expected 100 tokens, got %d", took)
	}
}

// TestBucket_NilSafeguards — NewBucket(0) returns nil; nil-receiver
// methods are no-ops so callers don't have to guard.
func TestBucket_NilSafeguards(t *testing.T) {
	var b *Bucket // nil
	if b != NewBucket(0) {
		t.Error("NewBucket(0) should return nil")
	}
	if ok, _ := b.Take(1000); !ok {
		t.Error("nil bucket should allow")
	}
	b.Refund(1000) // must not panic
}

// TestBucketStore_RebuildsOnLimitChange — a change to a key's limit
// resets the bucket; the cached counter would otherwise misreport
// capacity.
func TestBucketStore_RebuildsOnLimitChange(t *testing.T) {
	s := NewBucketStore()
	rpm1, _ := s.For("k1", 30, 0)
	rpm2, _ := s.For("k1", 60, 0)
	if rpm1 == rpm2 {
		t.Fatal("expected a new bucket when capacity changed")
	}
}

// TestRateLimitMiddleware_429OnExhaustion — drains the RPM bucket and
// verifies the 61st call returns 429 with the documented body shape.
func TestRateLimitMiddleware_429OnExhaustion(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenSQLite(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()

	buckets := NewBucketStore()

	key := &store.APIKey{
		ID:       "k_rl",
		Hash:     "h",
		Scope:    "user",
		UserID:   "alice",
		RPMLimit: 2,
	}
	downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the body so middleware's NopCloser is consumed (mimics
		// the real chat handler).
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	mw := RateLimitMiddleware(buckets)(downstream)

	makeReq := func() *http.Request {
		body := []byte(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		return req.WithContext(auth.WithTestKey(req.Context(), key))
	}

	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		mw.ServeHTTP(w, makeReq())
		if w.Code != http.StatusOK {
			t.Fatalf("admit #%d: got %d, want 200", i+1, w.Code)
		}
	}
	w := httptest.NewRecorder()
	mw.ServeHTTP(w, makeReq())
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("3rd admit: got %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 response should carry Retry-After")
	}
	var body struct {
		Error struct {
			Type      string `json:"type"`
			LimitType string `json:"limit_type"`
			Limit     int    `json:"limit"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 429 body: %v", err)
	}
	if body.Error.Type != "rate_limited" || body.Error.LimitType != "rpm" || body.Error.Limit != 2 {
		t.Errorf("unexpected error body: %+v", body.Error)
	}
}
