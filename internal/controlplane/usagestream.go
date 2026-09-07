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
	"github.com/opod-io/opod/pkg/adminapi"
	"net/http"
	"strconv"
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
	events := make([]adminapi.UsageEvent, 0, len(rows))
	next := after
	for _, u := range rows {
		events = append(events, adminapi.UsageEvent{
			ID: u.ID, Type: "usage", TS: u.TS,
			Data: adminapi.UsageData{
				APIKeyID: u.APIKeyID, UserID: u.UserID, Model: u.Model, Protocol: u.Protocol,
				PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, LatencyMS: u.LatencyMS,
				Outcome: u.Outcome, CostUSD: u.CostUSD, NodeID: u.NodeID,
			},
		})
		next = u.ID
	}
	writeJSON(w, http.StatusOK, adminapi.UsageBatch{Events: events, Next: next, More: more, Boot: bootUnix})
}
