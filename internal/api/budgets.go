package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

// BudgetMiddleware refuses requests whose API key has at least one
// active budget that's already at or above its limit. Reads ALL
// budgets for the key on each request, so multiple simultaneous
// budgets ("$10/day AND $100/month") compose with AND semantics —
// whichever runs out first blocks.
//
// Reset is lazy: before the admission check the middleware rolls any
// expired budgets via ResetExpired. That keeps us out of cron land —
// fresh state is observed on the next request regardless of how long
// the leader was offline.
//
// On overflow, returns HTTP 429 with `error.type = "budget_exceeded"`
// and a `unit`/`window`/`current`/`limit` payload so clients can show
// a meaningful "you've spent $100 this month" message.
//
// Audit: every refusal records a `budget_exceeded` audit row so admins
// can see who hit which budget and when.
func BudgetMiddleware(st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFrom(r.Context())
			if key == nil {
				next.ServeHTTP(w, r)
				return
			}
			// Roll expired windows BEFORE reading current_value so the
			// budget we read reflects the active window.
			_ = st.Budgets().ResetExpired(r.Context(), key.ID, time.Now())

			budgets, err := st.Budgets().ListByKey(r.Context(), key.ID)
			if err != nil || len(budgets) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			for _, b := range budgets {
				if b.CurrentValue >= b.LimitValue {
					recordBudgetRefusal(r, st, key, b)
					writeBudgetExceeded(w, b)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeBudgetExceeded(w http.ResponseWriter, b store.Budget) {
	retry := int(time.Until(b.ResetAt).Seconds())
	if retry < 60 {
		retry = 60
	}
	w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
	w.Header().Set(HeaderBudgetResetAt, formatResetAt(b.ResetAt))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	body := map[string]any{
		"error": map[string]any{
			"type":        "budget_exceeded",
			"message":     fmt.Sprintf("%s budget (%s) exhausted: %.4f / %.4f", b.Window, b.LimitUnit, b.CurrentValue, b.LimitValue),
			"unit":        b.LimitUnit,
			"window":      b.Window,
			"current":     b.CurrentValue,
			"limit":       b.LimitValue,
			"reset_at":    b.ResetAt,
			"retry_after": retry,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

func recordBudgetRefusal(r *http.Request, st store.Store, key *store.APIKey, b store.Budget) {
	if st == nil {
		return
	}
	rid := RequestIDFrom(r.Context())
	meta := fmt.Sprintf(`{"request_id":"%s","key_id":"%s","window":"%s","unit":"%s","current":%.4f,"limit":%.4f}`,
		rid, key.ID, b.Window, b.LimitUnit, b.CurrentValue, b.LimitValue)
	_ = st.Audit().Record(r.Context(), store.AuditEntry{
		TS:       time.Now(),
		Actor:    key.UserID,
		Action:   "budget_exceeded",
		Target:   fmt.Sprintf("budget:%d", b.ID),
		Metadata: meta,
	})
}

// incrementBudgetsAfterUsage walks the key's budgets and bumps each
// one by the usage delta (tokens or USD, per the budget's unit).
// Called from recordUsage once the real cost / token counts are known.
//
// Best-effort: a write failure here doesn't surface to the caller.
// On the next request the same budget is re-read with its prior
// (correct, pre-this-request) value, so the only loss is that this
// particular request's spend wasn't counted — small enough that
// retrying / cross-checking isn't worth the complexity.
func incrementBudgetsAfterUsage(ctx context.Context, st store.Store, keyID string, tokens int64, costUSD float64) {
	if st == nil || keyID == "" || (tokens == 0 && costUSD == 0) {
		return
	}
	budgets, err := st.Budgets().ListByKey(ctx, keyID)
	if err != nil {
		return
	}
	for _, b := range budgets {
		var delta float64
		switch b.LimitUnit {
		case "tokens":
			delta = float64(tokens)
		case "usd":
			delta = costUSD
		default:
			continue
		}
		if delta <= 0 {
			continue
		}
		_ = st.Budgets().Increment(ctx, b.ID, delta)
	}
}
