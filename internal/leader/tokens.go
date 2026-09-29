package leader

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/auth"
)

// ---- token admin ----

type tokenView struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Scope     string     `json:"scope"`
	UserID    string     `json:"user_id"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Revoked   bool       `json:"revoked"`
	CreatedAt time.Time  `json:"created_at"`
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
			Revoked: k.Revoked, CreatedAt: k.CreatedAt,
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
		Name       string     `json:"name"`
		Scope      string     `json:"scope"` // admin | user | node
		UserID     string     `json:"user_id"`
		ExpiresAt  *time.Time `json:"expires_at"`
		TTLSeconds int        `json:"ttl_seconds"`
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
		"id":         rec.ID,
		"name":       rec.Name,
		"scope":      rec.Scope,
		"plaintext":  plain, // shown ONCE; caller must save it now
		"created_at": rec.CreatedAt,
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
	// Expiry is the one editable field: a key is an identity with a scope and
	// a lifetime, and the per-key policy that used to be edited here (the
	// allowlist, the per-minute ceilings) left core on 2026-09-28 (ADR-077 §5).
	var req struct {
		ExpiresAt  *json.RawMessage `json:"expires_at"` // RFC3339 string or null
		TTLSeconds *int             `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.ExpiresAt == nil && req.TTLSeconds == nil {
		writeJSONError(w, http.StatusBadRequest, "no editable fields in body (try `expires_at`, `ttl_seconds`; per-key limits and allowlists are not this runtime's to hold)")
		return
	}
	resp := map[string]any{"id": id}
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
