package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/router"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/store"
)

// rateLimitEstimateKey is unexported so other packages can't shadow the
// estimate (which is meaningful only to the rate-limit reconciliation
// path).
type rateLimitEstimateKey struct{}

// rateLimitEstimate is the upfront token count attached to a request
// by RateLimitMiddleware. recordUsage reads it to reconcile against
// the actual prompt+completion tokens once the response is done.
type rateLimitEstimate struct {
	KeyID    string
	Estimate int
}

// WithRateLimitEstimate stashes the upfront estimate on ctx for the
// downstream recordUsage reconciliation step. Exported so the
// middleware (in the same package, but kept callable for tests too) can
// build the context.
func WithRateLimitEstimate(ctx context.Context, keyID string, estimate int) context.Context {
	return context.WithValue(ctx, rateLimitEstimateKey{}, rateLimitEstimate{KeyID: keyID, Estimate: estimate})
}

// rateLimitEstimateFrom reads the stashed estimate. Returns the zero
// value when no estimate was set — the reconciliation step is then a
// no-op.
func rateLimitEstimateFrom(ctx context.Context) rateLimitEstimate {
	v, _ := ctx.Value(rateLimitEstimateKey{}).(rateLimitEstimate)
	return v
}

// usageWriteTimeout bounds the usage insert once it is detached from the
// request: long enough for a busy SQLite writer, short enough that a wedged
// store cannot pile up handler goroutines.
const usageWriteTimeout = 5 * time.Second

// recordUsage writes a usage row for a finished request and updates metrics.
// Best-effort — failures are not surfaced to the caller (the request already
// ended from the user's perspective).
//
// The row never rides the request's own context: on the cancellation path
// (client gone mid-stream) that context is already done and the driver would
// refuse the insert, so exactly the requests that were cut short would go
// unrecorded. The write keeps the context's values and gets its own deadline.
//
// Metrics always fire (even when no API key is in context — e.g., dev mode
// with require_keys=false). The DB row is written with empty key/user
// identifiers in that case; the per-key index simply has more empty-string
// rows but everything stays observable.
func (h *Handler) recordUsage(ctx context.Context, protocol, model string,
	u *engines.Usage, latency time.Duration, outcome string) {
	h.recordUsageTTFT(ctx, protocol, model, u, latency, 0, outcome)
}

// recordUsageTTFT is recordUsage with the time to the first usable byte, which
// only the streaming path can know (R15.13). 0 = not streamed, and the control
// plane must not read it as "instant".
func (h *Handler) recordUsageTTFT(ctx context.Context, protocol, model string,
	u *engines.Usage, latency, ttft time.Duration, outcome string) {
	st, pol := h.Store, h.Policy()

	var keyID, userID string
	if k := auth.KeyFrom(ctx); k != nil {
		keyID = k.ID
		userID = k.UserID
	}

	prompt, completion := 0, 0
	if u != nil {
		prompt = u.PromptTokens
		completion = u.CompletionTokens
	}

	// Metrics first — always, regardless of auth state. Bound the label
	// cardinality: on non-ok outcomes the model string may be raw client
	// input that never resolved against the catalog, so label those
	// "unknown". The usage row below keeps the raw string — it's useful
	// for debugging there and the DB column isn't a Prometheus label.
	metricsModel := model
	if outcome != "ok" && !pol.modelInCatalog(model) {
		metricsModel = "unknown"
	}
	metrics.ObserveRequest(metricsModel, protocol, outcome, latency, prompt, completion)
	metrics.ObserveTTFT(metricsModel, ttft)

	rec := store.Usage{
		TS:               time.Now(),
		APIKeyID:         keyID,
		UserID:           userID,
		Model:            model,
		Protocol:         protocol,
		PromptTokens:     prompt,
		CompletionTokens: completion,
		LatencyMS:        int(latency.Milliseconds()),
		Outcome:          outcome,
		CostUSD:          0,                    // dollar cost left core with budgets (ADR-022); rating happens downstream
		NodeID:           router.NodeFrom(ctx), // "" when answered locally / never dispatched
		TTFTMS:           int(ttft.Milliseconds()),
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), usageWriteTimeout)
	defer cancel()
	if err := st.Usage().Record(writeCtx, rec); err != nil {
		// A store outage must not affect user-visible behaviour, but a lost
		// usage row is never silent.
		slog.Warn("usage: row not recorded", "model", model, "outcome", outcome, "err", err)
	}

	// A gateway replica's local row is its own view, not the record: hand the
	// row to the door's pusher so the leader — the single writer — gets it
	// (ADR-063). The request id is already unique per request and is what the
	// leader dedups on, so a resend after a failed push cannot double-count.
	if h.OnUsage != nil {
		id := RequestIDFrom(ctx)
		if id == "" {
			id = newRequestID()
		}
		h.OnUsage(rec, id)
	}

	// Reconcile the rate-limit TPM bucket. The middleware deducted an
	// upfront estimate; once the real usage is known we either refund
	// (over-estimated) or deduct the delta (under-estimated). The
	// bucket can go briefly negative — that's fine; subsequent
	// requests refill and rate-limit normally.
	if est := rateLimitEstimateFrom(ctx); est.KeyID != "" && pol.Buckets != nil {
		actual := prompt + completion
		if _, tpm := pol.Buckets.Get(est.KeyID); tpm != nil {
			switch {
			case actual > est.Estimate:
				tpm.Deduct(float64(actual - est.Estimate))
			case actual < est.Estimate:
				tpm.Refund(float64(est.Estimate - actual))
			}
		}
	}
}

// QuotaMiddleware enforces per-key daily token quotas. Keys with quota=0
// are unlimited.
func QuotaMiddleware(st store.Store) func(http.Handler) http.Handler {
	return QuotaMiddlewareShared(st, nil)
}

// SpendSource answers "is this key over its daily quota" from a view WIDER than
// this process's store. A gateway replica's own rows are a fraction of the
// key's day (ADR-063): asked locally, every door would let the key through
// until it had spent the whole quota at that one door. nil = a single writer,
// where the local store IS the day.
type SpendSource interface {
	QuotaExceeded(apiKeyID string) (over bool, used, quota int64)
}

// QuotaMiddlewareShared is QuotaMiddleware with an optional wider spend view.
func QuotaMiddlewareShared(st store.Store, spend SpendSource) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFrom(r.Context())
			if key == nil || key.QuotaDailyTokens <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			if spend != nil {
				// The snapshot is the authority on a door, stale or not: a door
				// that fell back to its own store would enforce a quota N times
				// too large, which is the bug this exists to avoid. How stale it
				// may be is published (spend_lag_bound_s on /loadz).
				if over, used, quota := spend.QuotaExceeded(key.ID); over {
					writeQuotaExceeded(w, quota, used)
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			midnight := startOfUTCDay(time.Now())
			used, err := st.Usage().SumTokensSince(r.Context(), key.ID, midnight)
			if err == nil && used >= key.QuotaDailyTokens {
				writeQuotaExceeded(w, key.QuotaDailyTokens, used)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func startOfUTCDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func writeQuotaExceeded(w http.ResponseWriter, quota, used int64) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "3600")
	w.WriteHeader(http.StatusTooManyRequests)
	body := map[string]any{
		"error": map[string]any{
			"type":    "rate_limit_error",
			"message": "Daily token quota exceeded",
			"quota":   quota,
			"used":    used,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}
