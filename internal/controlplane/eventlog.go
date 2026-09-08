package controlplane

// Durable typed lifecycle events (§13 item 1, second half — usage was first).
// Appended at the moments the fleet actually changes shape; pulled by an
// external collector over the same cursor contract as the usage stream:
//
//	GET /admin/v1/events/stream?after=<id>&limit=<n≤1000>
//	→ {"events":[{"id","type","ts","data":{…}}], "next", "more"}
//
// Types today: node.registered · node.drained · node.removed ·
// model.loaded · model.unloaded · shard.created · shard.removed ·
// plan.updated. Additive — consumers must ignore unknown types.

import (
	"github.com/opod-io/opod-sdk/adminapi"
	"net/http"
	"strconv"
)

// logEvent appends one lifecycle event; best-effort (a full disk must not
// break a heartbeat).
func (s *Server) logEvent(typ, subject string, data map[string]any) {
	if err := s.store.EventLog().Append(typ, subject, data); err != nil {
		s.log.Warn("event log append failed", "type", typ, "err", err)
	}
}

func (s *Server) eventLogStream(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit := 500
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 && v <= 1000 {
		limit = v
	}
	rows, err := s.store.EventLog().After(r.Context(), after, limit+1)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	events := make([]adminapi.LifecycleEvent, 0, len(rows))
	next := after
	for _, e := range rows {
		events = append(events, adminapi.LifecycleEvent{ID: e.ID, Type: e.Type, Subject: e.Subject, TS: e.TS, Data: e.Data})
		next = e.ID
	}
	writeJSON(w, http.StatusOK, adminapi.EventsBatch{Events: events, Next: next, More: more, Boot: bootUnix})
}
