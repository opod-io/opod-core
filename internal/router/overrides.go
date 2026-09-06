package router

import "context"

// Overrides carry per-request routing knobs that the OpenAI/Anthropic
// HTTP handlers extract from `opod.*` body fields or `X-Opod-*`
// headers. They override the catalog-level fallback chain for that one
// request only; an unset Overrides value (the zero value) preserves the
// pre-existing catalog-only behavior.
//
// Caps are enforced at parse time so a runaway request can't burn
// minutes on retries:
//
//   - NumRetries     ∈ [0, MaxRetries]
//   - RetryBackoffMS ∈ [0, RetryBackoffCapMS]
type Overrides struct {
	// Fallbacks, when non-empty, replaces the catalog fallback chain
	// for this request. Chain order is [primary, ...Fallbacks].
	Fallbacks []string

	// NumRetries, when > 0, retries each candidate up to N additional
	// times before advancing to the next candidate. Retries apply only
	// to synchronous Engine.Chat / Engine.Embed errors — once a stream
	// has started producing events, mid-stream errors propagate as-is.
	NumRetries int

	// RetryBackoffMS is the initial backoff between retries (ms). The
	// router doubles it after each failed attempt, capped at
	// RetryBackoffCapMS. 0 means "retry immediately" — usually you
	// want at least 100ms.
	RetryBackoffMS int

	// Hedge, when true, asks the router to fire the request to the
	// top-N least-loaded workers concurrently and return whichever
	// responds first. The N value comes from the router's
	// SetHedgeReplicas config; per-request override is a yes/no
	// toggle, not a count. Hedging skips the retry/fallback path —
	// the operator has already paid the N× cost.
	Hedge bool

	// Sort reorders the candidate chain by a metric instead of walking
	// it in catalog-preference order: "price" (cheapest first — free
	// local models beat vendor egress), "latency" (lowest rolling p95
	// first), or "throughput" (highest tokens/sec first). Set from
	// `opod.sort` / X-Opod-Sort, or via the `:floor` (price) and
	// `:nitro` (throughput) model-name suffixes. Empty = unset.
	Sort string
}

// MaxRetries is the upper bound the parsers enforce on
// `opod.num_retries`. Keeps a misconfigured client from burning a
// pathological number of retries before failing.
const MaxRetries = 5

// RetryBackoffCapMS is the ceiling for the doubled backoff between
// retries — past this, the doubling plateaus.
const RetryBackoffCapMS = 5000

// IsSet reports whether any override field is non-zero. Cheaper than
// reflecting and lets callers short-circuit the override path entirely
// for the (vast) majority of requests that don't customize routing.
func (o Overrides) IsSet() bool {
	return len(o.Fallbacks) > 0 || o.NumRetries > 0 || o.Hedge || o.Sort != ""
}

// ctxKey is unexported to avoid context-key collisions.
type ctxKey struct{}

// WithOverrides attaches `o` to `ctx`. Pass the resulting context to
// Router.Chat / Router.Embed so the router can see the request-level
// knobs.
func WithOverrides(ctx context.Context, o Overrides) context.Context {
	return context.WithValue(ctx, ctxKey{}, o)
}

// FromContext returns the overrides attached to ctx (zero value if
// none).
func FromContext(ctx context.Context) Overrides {
	v, _ := ctx.Value(ctxKey{}).(Overrides)
	return v
}

// Clamp enforces the documented caps. Callers can call this on a value
// parsed from untrusted client input. Negative values are floored at 0.
func (o Overrides) Clamp() Overrides {
	if o.NumRetries < 0 {
		o.NumRetries = 0
	}
	if o.NumRetries > MaxRetries {
		o.NumRetries = MaxRetries
	}
	if o.RetryBackoffMS < 0 {
		o.RetryBackoffMS = 0
	}
	if o.RetryBackoffMS > RetryBackoffCapMS {
		o.RetryBackoffMS = RetryBackoffCapMS
	}
	switch o.Sort {
	case "", SortPrice, SortLatency, SortThroughput:
	default:
		o.Sort = "" // unknown mode — ignore rather than 400 (forward compat)
	}
	return o
}
