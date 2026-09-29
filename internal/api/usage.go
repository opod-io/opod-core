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
