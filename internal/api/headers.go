package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// Header names — OpenAI-style, stamped by one middleware on every /v1 route.
const (
	HeaderRequestID = "X-Opod-Request-Id"
)

type requestIDKey struct{}

// WithRequestID stashes a freshly-generated id on ctx so the audit
// recorder and the response writer can reference the same identifier.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request id attached to ctx (or "").
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(requestIDKey{}).(string)
	return v
}

// newRequestID returns a 16-hex-char (128-bit) random id. Compact
// enough to print in a 80-column terminal but with enough entropy that
// collisions across the lifetime of a single leader are astronomically
// unlikely.
func newRequestID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return "req_" + hex.EncodeToString(buf)
}

// ResponseHeadersMiddleware stamps an `x-opod-request-id` correlation token
// on every response from a `/v1/*` route and puts the same id on the context
// for the usage row and the audit recorder. It writes BEFORE handing off to
// `next` so the header is present even on a streaming response.
//
// It used to carry `x-ratelimit-*` too, from per-key RPM/TPM buckets; those
// left core with the per-key policy (ADR-077 §5, 2026-09-28): rate limits,
// quotas and allowlists per caller are the application layer's in front of
// an endpoint, not the runtime's.
func ResponseHeadersMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := newRequestID()
			ctx := WithRequestID(r.Context(), id)
			w.Header().Set(HeaderRequestID, id)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
