package controlplane

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

// ---- token admin ----

type tokenView struct {
	ID               string     `json:"id"`
	Name             string     `json:"name"`
	Scope            string     `json:"scope"`
	UserID           string     `json:"user_id"`
	QuotaDailyTokens int64      `json:"quota_daily_tokens"`
	RPMLimit         int        `json:"rpm_limit"`
	TPMLimit         int        `json:"tpm_limit"`
	AllowedModels    []string   `json:"allowed_models"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
	Revoked          bool       `json:"revoked"`
	CreatedAt        time.Time  `json:"created_at"`
}

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	keys, err := s.store.APIKeys().List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]tokenView, 0, len(keys))
	for _, k := range keys {
		view := tokenView{
			ID: k.ID, Name: k.Name, Scope: k.Scope, UserID: k.UserID,
			QuotaDailyTokens: k.QuotaDailyTokens,
			RPMLimit:         k.RPMLimit,
			TPMLimit:         k.TPMLimit,
			AllowedModels:    k.AllowedModels,
			Revoked:          k.Revoked, CreatedAt: k.CreatedAt,
		}
		if !k.ExpiresAt.IsZero() {
			t := k.ExpiresAt
			view.ExpiresAt = &t
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Name             string     `json:"name"`
		Scope            string     `json:"scope"` // admin | user | node
		UserID           string     `json:"user_id"`
		QuotaDailyTokens int64      `json:"quota_daily_tokens"`
		RPMLimit         int        `json:"rpm_limit"`
		TPMLimit         int        `json:"tpm_limit"`
		AllowedModels    []string   `json:"allowed_models"`
		ExpiresAt        *time.Time `json:"expires_at"`
		TTLSeconds       int        `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Name == "" {
		writeJSONError(w, http.StatusBadRequest, "name required")
		return
	}
	if req.Scope == "" {
		req.Scope = "user"
	}
	if req.Scope != "admin" && req.Scope != "user" && req.Scope != "node" {
		writeJSONError(w, http.StatusBadRequest, "scope must be admin|user|node")
		return
	}
	userID := req.UserID
	if userID == "" && req.Scope != "node" {
		userID = req.Name
	}
	plain, rec, err := auth.Generate(req.Name, req.Scope, userID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	rec.QuotaDailyTokens = req.QuotaDailyTokens
	rec.RPMLimit = req.RPMLimit
	rec.TPMLimit = req.TPMLimit
	rec.AllowedModels = req.AllowedModels
	switch {
	case req.TTLSeconds > 0:
		rec.ExpiresAt = time.Now().Add(time.Duration(req.TTLSeconds) * time.Second)
	case req.ExpiresAt != nil:
		rec.ExpiresAt = req.ExpiresAt.UTC()
	}
	if err := s.store.APIKeys().Create(r.Context(), rec); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp := map[string]any{
		"id":             rec.ID,
		"name":           rec.Name,
		"scope":          rec.Scope,
		"rpm_limit":      rec.RPMLimit,
		"tpm_limit":      rec.TPMLimit,
		"allowed_models": rec.AllowedModels,
		"plaintext":      plain, // shown ONCE; caller must save it now
		"created_at":     rec.CreatedAt,
	}
	if !rec.ExpiresAt.IsZero() {
		resp["expires_at"] = rec.ExpiresAt
	}
	writeJSON(w, http.StatusOK, resp)
}

// editToken updates editable fields on an existing token. Today only
// the allowlist (`allowed_models`) is editable; revoke/delete is handled
// by DELETE. The body is a partial-update — pass `allowed_models: null`
// to remove the restriction, `[]` to deny all, or a list to replace.
func (s *Server) editToken(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "token id required")
		return
	}
	// Use a json.RawMessage for allowed_models so we can tell
	// "field absent" from "field present and null" — both round-trip
	// to a nil slice in a plain `[]string` field. RPM/TPM use
	// pointers (nil = field absent, *int = explicit set).
	var req struct {
		AllowedModels *json.RawMessage `json:"allowed_models"`
		RPMLimit      *int             `json:"rpm_limit"`
		TPMLimit      *int             `json:"tpm_limit"`
		ExpiresAt     *json.RawMessage `json:"expires_at"` // RFC3339 string or null
		TTLSeconds    *int             `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.AllowedModels == nil && req.RPMLimit == nil && req.TPMLimit == nil && req.ExpiresAt == nil && req.TTLSeconds == nil {
		writeJSONError(w, http.StatusBadRequest, "no editable fields in body (try `allowed_models`, `rpm_limit`, `tpm_limit`, `expires_at`, `ttl_seconds`)")
		return
	}
	resp := map[string]any{"id": id}
	if req.AllowedModels != nil {
		raw := string(*req.AllowedModels)
		var allowed []string // nil = unrestricted
		if raw != "null" {
			if err := json.Unmarshal(*req.AllowedModels, &allowed); err != nil {
				writeJSONError(w, http.StatusBadRequest, "allowed_models must be a list or null")
				return
			}
			if allowed == nil {
				allowed = []string{} // empty list = deny all
			}
		}
		if err := s.store.APIKeys().UpdateAllowedModels(r.Context(), id, allowed); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp["allowed_models"] = allowed
	}
	if req.RPMLimit != nil || req.TPMLimit != nil {
		// Fetch current values so a partial edit doesn't accidentally
		// reset the field not being changed.
		current, err := s.store.APIKeys().GetByID(r.Context(), id)
		if err != nil || current == nil {
			writeJSONError(w, http.StatusNotFound, "token not found")
			return
		}
		rpm, tpm := current.RPMLimit, current.TPMLimit
		if req.RPMLimit != nil {
			rpm = *req.RPMLimit
		}
		if req.TPMLimit != nil {
			tpm = *req.TPMLimit
		}
		if err := s.store.APIKeys().UpdateRateLimits(r.Context(), id, rpm, tpm); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		resp["rpm_limit"] = rpm
		resp["tpm_limit"] = tpm
	}
	if req.ExpiresAt != nil || req.TTLSeconds != nil {
		var expiresAt time.Time
		switch {
		case req.TTLSeconds != nil:
			if *req.TTLSeconds <= 0 {
				expiresAt = time.Time{} // "never expires"
			} else {
				expiresAt = time.Now().Add(time.Duration(*req.TTLSeconds) * time.Second)
			}
		case req.ExpiresAt != nil:
			raw := string(*req.ExpiresAt)
			if raw == "null" {
				expiresAt = time.Time{}
			} else {
				var ts time.Time
				if err := json.Unmarshal(*req.ExpiresAt, &ts); err != nil {
					writeJSONError(w, http.StatusBadRequest, "expires_at must be an RFC3339 timestamp or null")
					return
				}
				expiresAt = ts.UTC()
			}
		}
		if err := s.store.APIKeys().UpdateExpiresAt(r.Context(), id, expiresAt); err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if expiresAt.IsZero() {
			resp["expires_at"] = nil
		} else {
			resp["expires_at"] = expiresAt
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

// listBudgets returns every budget currently attached to the given
// API key id. Lazily rolls expired windows before reading so the
// dashboard sees fresh current_value / reset_at fields.
func (s *Server) listBudgets(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	_ = s.store.Budgets().ResetExpired(r.Context(), id, time.Now())
	bs, err := s.store.Budgets().ListByKey(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if bs == nil {
		bs = []store.Budget{}
	}
	writeJSON(w, http.StatusOK, bs)
}

// createBudget attaches a new spend/token budget to an API key.
//
//	POST /admin/v1/tokens/{id}/budgets
//	  { "window": "day|week|month", "limit_unit": "tokens|usd", "limit_value": 100 }
func (s *Server) createBudget(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	id := chi.URLParam(r, "id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "token id required")
		return
	}
	var req struct {
		Window     string  `json:"window"`
		LimitUnit  string  `json:"limit_unit"`
		LimitValue float64 `json:"limit_value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	switch req.Window {
	case "day", "week", "month":
	default:
		writeJSONError(w, http.StatusBadRequest, "window must be day|week|month")
		return
	}
	switch req.LimitUnit {
	case "tokens", "usd":
	default:
		writeJSONError(w, http.StatusBadRequest, "limit_unit must be tokens|usd")
		return
	}
	if req.LimitValue <= 0 {
		writeJSONError(w, http.StatusBadRequest, "limit_value must be > 0")
		return
	}
	b := store.Budget{
		APIKeyID:   id,
		Window:     req.Window,
		LimitUnit:  req.LimitUnit,
		LimitValue: req.LimitValue,
	}
	bid, err := s.store.Budgets().Create(r.Context(), b)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	b.ID = bid
	b.ResetAt = store.NextBudgetReset(b.Window, time.Now())
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) deleteBudget(w http.ResponseWriter, r *http.Request) {
	bidStr := chi.URLParam(r, "bid")
	bid, err := strconv.ParseInt(bidStr, 10, 64)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid budget id")
		return
	}
	if err := s.store.Budgets().Delete(r.Context(), bid); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "removed", "id": bid})
}

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.APIKeys().Revoke(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "id": id})
}
