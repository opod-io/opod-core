package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

func TestResponseHeadersMiddleware_AlwaysEmitsRequestID(t *testing.T) {
	buckets := NewBucketStore()
	mw := ResponseHeadersMiddleware(buckets)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Confirm the id is in ctx by the time downstream sees it.
		if RequestIDFrom(r.Context()) == "" {
			t.Error("downstream got no request_id in ctx")
		}
		w.WriteHeader(http.StatusOK)
	}))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	got := w.Header().Get(HeaderRequestID)
	if !strings.HasPrefix(got, "req_") || len(got) < 12 {
		t.Errorf("X-Opod-Request-Id = %q, want req_<hex>", got)
	}
}

func TestResponseHeadersMiddleware_EmitsRateLimitWhenBucketsExist(t *testing.T) {
	buckets := NewBucketStore()
	// Initialize the buckets up-front so the header writer can read them.
	// Mimics what RateLimitMiddleware would do on first admission.
	buckets.For("k_test", 60, 100000)

	mw := ResponseHeadersMiddleware(buckets)
	called := false
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	key := &store.APIKey{ID: "k_test", RPMLimit: 60, TPMLimit: 100000}
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(auth.WithTestKey(context.Background(), key))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if !called {
		t.Fatal("downstream not called")
	}
	if w.Header().Get(HeaderLimitRequests) != "60" {
		t.Errorf("limit-requests = %q, want 60", w.Header().Get(HeaderLimitRequests))
	}
	if w.Header().Get(HeaderLimitTokens) != "100000" {
		t.Errorf("limit-tokens = %q, want 100000", w.Header().Get(HeaderLimitTokens))
	}
	// Remaining should be at or near capacity since nothing's been taken.
	remRPM, _ := strconv.Atoi(w.Header().Get(HeaderRemainingRequests))
	if remRPM < 55 || remRPM > 60 {
		t.Errorf("remaining-requests = %d, want 55..60", remRPM)
	}
}

func TestResponseHeadersMiddleware_SkipsHeadersForUnlimitedKey(t *testing.T) {
	buckets := NewBucketStore()
	mw := ResponseHeadersMiddleware(buckets)
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	key := &store.APIKey{ID: "k_unlimited"} // RPMLimit=0, TPMLimit=0
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req = req.WithContext(auth.WithTestKey(context.Background(), key))

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if v := w.Header().Get(HeaderLimitRequests); v != "" {
		t.Errorf("limit-requests should be empty for unlimited key, got %q", v)
	}
	if v := w.Header().Get(HeaderRequestID); !strings.HasPrefix(v, "req_") {
		t.Errorf("request_id always emitted; got %q", v)
	}
}
