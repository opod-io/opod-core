package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// ModelAllowMiddleware enforces a per-key model allowlist.
//
// Behavior by allowlist state:
//   - nil (the column default for legacy keys, and unset on new keys):
//     no restriction — pass through.
//   - []string{}: deny every model — useful for hard-disabling a key
//     without revoking it.
//   - list: the request's `model` field must match one entry. Entries
//     ending in `*` are glob prefixes (e.g. `claude-*` matches every
//     Claude model id).
//
// On a refusal the middleware returns HTTP 403 with code
// `model_not_allowed` and the allowed list in the body, then records an
// audit entry so operators can see who tried what. GETs (e.g.
// `/v1/models`) bypass the body-read but are still filtered against the
// allowlist by the handler — this middleware only guards POSTs that
// carry a `model` field.
func ModelAllowMiddleware(st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := auth.KeyFrom(r.Context())
			if key == nil || key.AllowedModels == nil {
				next.ServeHTTP(w, r)
				return
			}
			if r.Method != http.MethodPost && r.Method != http.MethodPut && r.Method != http.MethodPatch {
				next.ServeHTTP(w, r)
				return
			}
			// Buffer the body so the downstream handler can re-read it.
			// The /v1 endpoints already do this internally; replaying via
			// NopCloser keeps that intact.
			body, err := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if err != nil {
				writeJSONErr(w, http.StatusBadRequest, "invalid_request", "read body: "+err.Error())
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))

			model := peekRequestedModel(body)
			if model == "" {
				// No model field — let the handler reject as malformed.
				next.ServeHTTP(w, r)
				return
			}
			// A `:floor`/`:nitro` routing suffix is not part of the model
			// identity — an allowlist of ["x"] authorizes "x:floor".
			model, _ = models.SplitSortSuffix(model)
			if !ModelAllowed(key.AllowedModels, model) {
				auditRefusal(r.Context(), st, key, model)
				writeModelNotAllowed(w, model, key.AllowedModels)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// modelAllowedForKey re-checks the per-key allowlist against `model`
// using the API key on ctx. Handlers call this after default/auto
// substitution — ModelAllowMiddleware only sees the model the client
// actually sent, so an omitted model would otherwise bypass the
// allowlist via the configured default. A `:floor`/`:nitro` routing
// suffix is stripped first, consistent with the middleware. Returns
// true when no key (or no restriction) is attached.
func modelAllowedForKey(ctx context.Context, model string) bool {
	key := auth.KeyFrom(ctx)
	if key == nil || key.AllowedModels == nil {
		return true
	}
	model, _ = models.SplitSortSuffix(model)
	return ModelAllowed(key.AllowedModels, model)
}

// ModelAllowed reports whether `model` matches any entry in `allowed`.
// An entry may be a literal id or a `*`-suffixed glob (e.g. `claude-*`).
// A nil allowed slice means "no restriction" — the caller is expected
// to short-circuit before calling, but we return true for safety.
func ModelAllowed(allowed []string, model string) bool {
	if allowed == nil {
		return true
	}
	for _, pat := range allowed {
		if pat == model {
			return true
		}
		if pat == "*" {
			return true
		}
		if strings.HasSuffix(pat, "*") && strings.HasPrefix(model, strings.TrimSuffix(pat, "*")) {
			return true
		}
	}
	return false
}

func peekRequestedModel(body []byte) string {
	var m struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &m)
	return m.Model
}

func writeModelNotAllowed(w http.ResponseWriter, requested string, allowed []string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	body := map[string]any{
		"error": map[string]any{
			"type":           "model_not_allowed",
			"message":        fmt.Sprintf("API key is not authorized for model %q", requested),
			"requested":      requested,
			"allowed_models": allowed,
		},
	}
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"type": code, "message": msg},
	})
}

func auditRefusal(ctx context.Context, st store.Store, key *store.APIKey, model string) {
	if st == nil {
		return
	}
	meta := ""
	if rid := RequestIDFrom(ctx); rid != "" {
		meta = `{"request_id":"` + rid + `"}`
	}
	_ = st.Audit().Record(ctx, store.AuditEntry{
		TS:       time.Now(),
		Actor:    key.UserID,
		Action:   "model_not_allowed",
		Target:   model,
		Metadata: meta,
	})
}
