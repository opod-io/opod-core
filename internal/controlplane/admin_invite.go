package controlplane

import (
	"encoding/json"
	"net/http"

	"github.com/opod-io/opod/internal/control"
)

// inviteUser serves POST /admin/v1/invite.
// Body: {name, quota_daily_tokens?, clients?[], model?, base_url?}.
// Wraps control.Invite — the same Go function `opod invite` uses,
// so the CLI and dashboard produce identical store state and the same
// share card.
func (s *Server) inviteUser(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		Name             string   `json:"name"`
		QuotaDailyTokens int64    `json:"quota_daily_tokens"`
		Clients          []string `json:"clients"`
		Model            string   `json:"model"`
		BaseURL          string   `json:"base_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.BaseURL == "" {
		req.BaseURL = s.defaultBaseURL(r)
	}
	res, err := control.Invite(r.Context(), s.store, control.InviteInput{
		Name:             req.Name,
		BaseURL:          req.BaseURL,
		QuotaDailyTokens: req.QuotaDailyTokens,
		Clients:          req.Clients,
		Model:            req.Model,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Encode snippets as a flat map[string]string for easier UI consumption.
	snippets := make(map[string]string, len(res.Snippets))
	for k, v := range res.Snippets {
		snippets[k] = v.Snippet
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"token":              res.Token, // shown ONCE
		"token_id":           res.Record.ID,
		"name":               res.Record.Name,
		"scope":              res.Record.Scope,
		"quota_daily_tokens": res.Record.QuotaDailyTokens,
		"created_at":         res.Record.CreatedAt,
		"base_url":           res.BaseURL,
		"snippets":           snippets,
		"clients_order":      res.ClientsOrder,
		"markdown_card":      res.MarkdownCard(),
	})
}
