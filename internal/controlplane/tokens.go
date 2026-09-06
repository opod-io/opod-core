package controlplane

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/auth"
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

func (s *Server) revokeToken(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.APIKeys().Revoke(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "revoked", "id": id})
}
