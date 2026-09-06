package controlplane

// usageStream is the durable, cursor-based pull an external collector uses to
// capture this leader's usage spool with at-least-once semantics (§13 item 1,
// the first typed stream). The usage table's AUTOINCREMENT id is the cursor:
// monotonic, never reused, survives leader restarts because SQLite is the
// spool. The caller stores the last id it durably committed and asks for
// `?after=<that id>`; replaying after a crash re-reads rows it already has
// (dedupe on the caller's side), never loses any.
//
//	GET /admin/v1/usage/stream?after=<id>&limit=<n≤1000>
//	→ {"events":[{"id","type":"usage","ts","data":{…row…}}], "next":<last id>, "more":bool}

import (
	"net/http"
	"strconv"
	"time"
)

func (s *Server) usageStream(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := 500
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	// Fetch one extra row to answer "more" without a second query.
	rows, err := s.store.Usage().After(r.Context(), after, limit+1)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	type event struct {
		ID   int64          `json:"id"`
		Type string         `json:"type"`
		TS   time.Time      `json:"ts"`
		Data map[string]any `json:"data"`
	}
	events := make([]event, 0, len(rows))
	next := after
	for _, u := range rows {
		events = append(events, event{
			ID: u.ID, Type: "usage", TS: u.TS,
			Data: map[string]any{
				"api_key_id":        u.APIKeyID,
				"user_id":           u.UserID,
				"model":             u.Model,
				"protocol":          u.Protocol,
				"prompt_tokens":     u.PromptTokens,
				"completion_tokens": u.CompletionTokens,
				"latency_ms":        u.LatencyMS,
				"outcome":           u.Outcome,
				"cost_usd":          u.CostUSD,
			},
		})
		next = u.ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events, "next": next, "more": more})
}
