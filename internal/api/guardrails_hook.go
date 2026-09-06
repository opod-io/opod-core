package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/guardrails"
	"github.com/opod-io/opod/internal/metrics"
	"github.com/opod-io/opod/internal/store"
)

// applyPreCallGuardrails walks the configured pre + logging_only
// chains over the request body. Returns the (possibly-rewritten) body
// and a bool: if false, the handler should stop — a `block` action
// has already been written to the response in the OpenAI error shape.
// The Anthropic handler uses evalPreCallGuardrails directly and writes
// its own protocol-shaped error.
func applyPreCallGuardrails(ctx context.Context, w http.ResponseWriter, st store.Store, body []byte) ([]byte, bool) {
	return ApplyPreCallGuardrails(ctx, w, st, body)
}

// ApplyPreCallGuardrails is the exported form for the controlplane's
// dispatcher: a request that leaves this leader (policy fallback, P12-2) must
// pass the same pre chain a served request does — a blocked prompt is never
// forwarded.
func ApplyPreCallGuardrails(ctx context.Context, w http.ResponseWriter, st store.Store, body []byte) ([]byte, bool) {
	current, blockedBy, reason := evalPreCallGuardrails(ctx, st, body)
	if blockedBy != "" {
		writeGuardrailBlocked(w, blockedBy, reason)
		return nil, false
	}
	return current, true
}

// evalPreCallGuardrails is the protocol-agnostic core of the pre-call
// hook. Returns the (possibly-rewritten) body plus — when a pre
// guardrail blocked the request — the blocking guardrail's name and
// reason. blockedBy == "" means the request may proceed.
//
// Order of evaluation: pre first (in declared order), then
// logging_only. A `block` from any pre guardrail short-circuits the
// chain; logging_only entries can't block. Each guardrail observes
// the latest body (so a `rewrite` from guardrail #1 is what
// guardrail #2 sees).
func evalPreCallGuardrails(ctx context.Context, st store.Store, body []byte) (out []byte, blockedBy, reason string) {
	reg := globalGuardrails.Load()
	if reg.IsEmpty() {
		return body, "", ""
	}
	current := body
	// Pre chain — block / rewrite / allow.
	if reg.Pre != nil {
		for _, g := range reg.Pre.Guards() {
			act, _ := g.Check(ctx, current)
			metrics.ObserveGuardrail(g.Name(), act.Kind)
			switch act.Kind {
			case "block":
				recordGuardrailAudit(ctx, st, g.Name(), "block", act.Reason)
				return nil, g.Name(), act.Reason
			case "rewrite":
				current = act.NewBody
			case "flag":
				recordGuardrailAudit(ctx, st, g.Name(), "flag", act.Reason)
			}
		}
	}
	// Logging-only — observe, never alter the body or block.
	if reg.LoggingOnly != nil {
		for _, g := range reg.LoggingOnly.Guards() {
			act, _ := g.Check(ctx, current)
			metrics.ObserveGuardrail(g.Name(), act.Kind)
			if act.Kind == "flag" || act.Kind == "block" {
				// `block` from a logging-only guardrail is downgraded
				// to a flag in the audit log; the request still passes.
				recordGuardrailAudit(ctx, st, g.Name(), "flag", act.Reason)
			}
		}
	}
	return current, "", ""
}

func writeGuardrailBlocked(w http.ResponseWriter, name, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	body := map[string]any{
		"error": map[string]any{
			"type":      "guardrail_blocked",
			"message":   "request blocked by guardrail",
			"guardrail": name,
			"reason":    reason,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

// writeAnthropicGuardrailBlocked mirrors writeGuardrailBlocked in the
// Anthropic error envelope so SDK clients surface the refusal cleanly.
func writeAnthropicGuardrailBlocked(w http.ResponseWriter, name, reason string) {
	msg := "request blocked by guardrail " + name
	if reason != "" {
		msg += ": " + reason
	}
	writeAnthropicError(w, http.StatusForbidden, "permission_error", msg)
}

func recordGuardrailAudit(ctx context.Context, st store.Store, name, action, reason string) {
	if st == nil {
		return
	}
	actor := ""
	if k := auth.KeyFrom(ctx); k != nil {
		actor = k.UserID
	}
	meta := `{"reason":` + jsonQuote(reason) + `,"request_id":"` + RequestIDFrom(ctx) + `"}`
	_ = st.Audit().Record(ctx, store.AuditEntry{
		Actor:    actor,
		Action:   "guardrail." + action,
		Target:   name,
		Metadata: meta,
	})
	// P12-3: verdict metadata (never content) also rides the lifecycle stream
	// so the manager's audit sees blocks and flags per rule.
	_ = st.EventLog().Append("guardrail."+action, name, map[string]any{"actor": actor, "reason": reason, "request_id": RequestIDFrom(ctx)})
}

// jsonQuote is a cheap escape for the audit metadata embed. Goes
// through json.Marshal to get the right escaping for quotes /
// newlines.
func jsonQuote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// ensure guardrails is imported when registry use compiles out.
var _ = (*guardrails.Registry)(nil)
