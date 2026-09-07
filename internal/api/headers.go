package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/opod-io/opod/internal/auth"
)

// Header names — OpenAI-style, stamped by one middleware on every /v1 route.
const (
	HeaderRequestID         = "X-Opod-Request-Id"
	HeaderBudgetResetAt     = "X-Opod-Budget-Reset-At"
	HeaderLimitRequests     = "X-RateLimit-Limit-Requests"
	HeaderRemainingRequests = "X-RateLimit-Remaining-Requests"
	HeaderResetRequests     = "X-RateLimit-Reset-Requests"
	HeaderLimitTokens       = "X-RateLimit-Limit-Tokens"
	HeaderRemainingTokens   = "X-RateLimit-Remaining-Tokens"
	HeaderResetTokens       = "X-RateLimit-Reset-Tokens"
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

// ResponseHeadersMiddleware emits standard rate-limit headers
// (`x-ratelimit-*`) and a `x-opod-request-id` correlation token on
// every response from a `/v1/*` route. Client SDKs (OpenAI's,
// Anthropic's wrappers) inspect these headers to surface throttling
// status and pick retry timing.
//
// The middleware does its writes BEFORE handing off to `next` so the
// values are present even on a streaming response (which writes
// headers as soon as Flush() runs). Reading bucket state at the top of
// the handler is correct for rate-limit accounting too — by the time
// the response is being assembled the per-request RPM/TPM deduction
// has already happened (RateLimitMiddleware runs earlier in the
// chain), so what we report here is the bucket's state AS OF this
// request's admission, which is what clients want for "remaining" to
// reflect this very request.
func ResponseHeadersMiddleware(buckets *BucketStore) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := newRequestID()
			ctx := WithRequestID(r.Context(), id)
			w.Header().Set(HeaderRequestID, id)

			key := auth.KeyFrom(r.Context())
			if key != nil && buckets != nil {
				rpm, tpm := buckets.Get(key.ID)
				if rpm != nil && key.RPMLimit > 0 {
					w.Header().Set(HeaderLimitRequests, strconv.Itoa(key.RPMLimit))
					w.Header().Set(HeaderRemainingRequests, strconv.Itoa(int(rpm.Available())))
					w.Header().Set(HeaderResetRequests, strconv.Itoa(rpm.RefillETA()))
				}
				if tpm != nil && key.TPMLimit > 0 {
					w.Header().Set(HeaderLimitTokens, strconv.Itoa(key.TPMLimit))
					w.Header().Set(HeaderRemainingTokens, strconv.Itoa(int(tpm.Available())))
					w.Header().Set(HeaderResetTokens, strconv.Itoa(tpm.RefillETA()))
				}
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// formatResetAt formats a future time for the
// `x-opod-budget-reset-at` header in RFC3339. Hoisted into its own
// helper so the budget overflow path and the response middleware emit
// the same string for the same time value.
func formatResetAt(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
