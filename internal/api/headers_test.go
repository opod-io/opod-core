package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResponseHeadersMiddleware_AlwaysEmitsRequestID(t *testing.T) {
	mw := ResponseHeadersMiddleware()
	var seen string
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	id := rec.Header().Get(HeaderRequestID)
	if id == "" || !strings.HasPrefix(id, "req_") {
		t.Fatalf("no request id on the response: %q", id)
	}
	if seen != id {
		t.Fatalf("the handler saw %q, the client %q: one id for the row and the response", seen, id)
	}
	// The per-key rate-limit headers left with the per-key policy (ADR-077 §5).
	for _, h := range []string{"X-RateLimit-Limit-Requests", "X-RateLimit-Remaining-Tokens"} {
		if rec.Header().Get(h) != "" {
			t.Fatalf("%s is still emitted", h)
		}
	}
}
