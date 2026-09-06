// Per-request routing override plumbing — the bridge from
// (HTTP body | X-Opod-* headers) to router.Overrides on ctx.
//
// Shared between the OpenAI and Anthropic handlers so a client can use
// the same `opod.fallbacks` / `X-Opod-Fallbacks` mechanism regardless
// of which protocol the rest of the request speaks.
package api

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/router"
	"github.com/opod-io/opod/internal/store"
)

// overridesContext extracts overrides from the request body extras +
// X-Opod-* headers, attaches them to a derived ctx, and records an
// audit row when the request actually carried overrides (so admins can
// see who's bypassing the catalog policy).
//
// sortHint is the sort mode parsed from a `:floor`/`:nitro` model-name
// suffix — lowest precedence (body field, then header, then suffix).
// st may be nil — the audit step is best-effort.
func overridesContext(r *http.Request, body *opodExtras, st store.Store, requestedModel, sortHint string) context.Context {
	o := mergeBodyAndHeaders(body, r.Header)
	if o.Sort == "" {
		o.Sort = sortHint
	}
	o = o.Clamp()
	if !o.IsSet() {
		return r.Context()
	}
	recordOverrideAudit(r.Context(), st, requestedModel, o)
	return router.WithOverrides(r.Context(), o)
}

// overridesContextAnthropic mirrors overridesContext but for the
// Anthropic handler, whose body shape carries the same fields under the
// same `opod` key.
func overridesContextAnthropic(r *http.Request, body *opodExtras, st store.Store, requestedModel, sortHint string) context.Context {
	return overridesContext(r, body, st, requestedModel, sortHint)
}

func mergeBodyAndHeaders(body *opodExtras, h http.Header) router.Overrides {
	o := router.Overrides{}
	if body != nil {
		o.Fallbacks = body.Fallbacks
		o.NumRetries = body.NumRetries
		o.RetryBackoffMS = body.RetryBackoffMS
		o.Hedge = body.Hedge
		o.Sort = body.Sort
	}
	// Headers fill in only when the body left a field zero. Body wins so
	// a client can be explicit about overriding a proxy-injected header.
	if len(o.Fallbacks) == 0 {
		if v := h.Get("X-Opod-Fallbacks"); v != "" {
			o.Fallbacks = splitAndTrim(v)
		}
	}
	if o.NumRetries == 0 {
		if v := h.Get("X-Opod-Num-Retries"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				o.NumRetries = n
			}
		}
	}
	if o.RetryBackoffMS == 0 {
		if v := h.Get("X-Opod-Retry-Backoff-Ms"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				o.RetryBackoffMS = n
			}
		}
	}
	if !o.Hedge {
		if v := h.Get("X-Opod-Hedge"); v == "1" || v == "true" {
			o.Hedge = true
		}
	}
	if o.Sort == "" {
		o.Sort = h.Get("X-Opod-Sort")
	}
	return o
}

func splitAndTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func recordOverrideAudit(ctx context.Context, st store.Store, requestedModel string, o router.Overrides) {
	if st == nil {
		return
	}
	keyID, userID := "", ""
	if k := auth.KeyFrom(ctx); k != nil {
		keyID = k.ID
		userID = k.UserID
	}
	meta := overrideAuditMetaWithReq(ctx, keyID, o)
	_ = st.Audit().Record(ctx, store.AuditEntry{
		TS:       time.Now(),
		Actor:    userID,
		Action:   "router.override",
		Target:   requestedModel,
		Metadata: meta,
	})
}

// overrideAuditMeta builds the JSON metadata payload so operators can
// see what the override actually was (fallbacks list, retry count) from
// the audit row without grepping logs.
func overrideAuditMeta(keyID string, o router.Overrides) string {
	parts := make([]string, 0, 5)
	if keyID != "" {
		parts = append(parts, kv("key_id", keyID))
	}
	if len(o.Fallbacks) > 0 {
		parts = append(parts, kv("fallbacks", strings.Join(o.Fallbacks, ",")))
	}
	if o.NumRetries > 0 {
		parts = append(parts, kv("num_retries", strconv.Itoa(o.NumRetries)))
	}
	if o.RetryBackoffMS > 0 {
		parts = append(parts, kv("retry_backoff_ms", strconv.Itoa(o.RetryBackoffMS)))
	}
	if o.Sort != "" {
		parts = append(parts, kv("sort", o.Sort))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// overrideAuditMetaWithReq is the request-aware variant that prepends
// the request id so audit rows correlate with x-opod-request-id on the
// response.
func overrideAuditMetaWithReq(ctx context.Context, keyID string, o router.Overrides) string {
	rid := RequestIDFrom(ctx)
	if rid == "" {
		return overrideAuditMeta(keyID, o)
	}
	parts := []string{kv("request_id", rid)}
	if keyID != "" {
		parts = append(parts, kv("key_id", keyID))
	}
	if len(o.Fallbacks) > 0 {
		parts = append(parts, kv("fallbacks", strings.Join(o.Fallbacks, ",")))
	}
	if o.NumRetries > 0 {
		parts = append(parts, kv("num_retries", strconv.Itoa(o.NumRetries)))
	}
	if o.RetryBackoffMS > 0 {
		parts = append(parts, kv("retry_backoff_ms", strconv.Itoa(o.RetryBackoffMS)))
	}
	if o.Sort != "" {
		parts = append(parts, kv("sort", o.Sort))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// kv renders one JSON object member. Key and value go through
// jsonQuote so client-controlled strings (model ids, fallback lists)
// can't inject extra JSON into the audit metadata.
func kv(k, v string) string {
	return jsonQuote(k) + ":" + jsonQuote(v)
}
