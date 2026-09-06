package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/opod-io/opod/internal/api"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

func (s *Server) listUsageRecent(w http.ResponseWriter, r *http.Request) {
	us, err := s.store.Usage().Recent(r.Context(), 200)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, us)
}

// usageBreakdown serves time-bucketed aggregates of the usage table.
//
//	GET /admin/v1/usage/breakdown
//	  ?bucket=hour|day|month|total      (default: day)
//	  &since=YYYY-MM-DD                  (default: 30 days ago)
//	  &until=YYYY-MM-DD                  (default: now)
//	  &group_by=user,model,protocol,outcome   (comma-separated, any order)
//	  &limit=N                           (0 = no cap)
//
// Response:
//
//	{
//	  "rows":   [{"bucket":"2026-06-09","user":"alice","model":"qwen3.6-27b","prompt_tokens":12345,"completion_tokens":6789,"requests":42}, …],
//	  "totals": {"prompt_tokens":…, "completion_tokens":…, "requests":…}
//	}
func (s *Server) usageBreakdown(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := store.BreakdownOpts{
		Bucket: q.Get("bucket"),
	}
	if v := q.Get("since"); v != "" {
		t, err := parseDateFlexible(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid since: "+err.Error())
			return
		}
		opts.Since = t
	}
	if v := q.Get("until"); v != "" {
		t, err := parseDateFlexible(v)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid until: "+err.Error())
			return
		}
		opts.Until = t
	}
	if v := q.Get("group_by"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				opts.GroupBy = append(opts.GroupBy, p)
			}
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			opts.Limit = n
		}
	}
	rows, totals, err := s.store.Usage().Breakdown(r.Context(), opts)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"rows":   rows,
		"totals": totals,
		"bucket": opts.Bucket,
		"since":  opts.Since,
		"until":  opts.Until,
	})
}

// parseDateFlexible accepts either a YYYY-MM-DD or an RFC3339 timestamp.
// Dates are interpreted in UTC at midnight so "since=2026-06-01" matches
// "the start of June 1".
func parseDateFlexible(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("expected YYYY-MM-DD or RFC3339, got %q", s)
}

func (s *Server) listAuditRecent(w http.ResponseWriter, r *http.Request) {
	es, err := s.store.Audit().Recent(r.Context(), 200)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, es)
}

// usageSummary computes aggregate stats from the last 1000 usage rows:
// total requests, top models, p50/p95/p99 latency, error rate, and a
// 60-minute requests-per-minute series suitable for a sparkline. All
// computed in-memory; the table is small enough that this stays cheap.
func (s *Server) usageSummary(w http.ResponseWriter, r *http.Request) {
	us, err := s.store.Usage().Recent(r.Context(), 1000)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type modelStat struct {
		Model   string  `json:"model"`
		Count   int     `json:"count"`
		CostUSD float64 `json:"cost_usd"`
	}
	out := struct {
		Total       int         `json:"total"`
		TokensTotal int64       `json:"tokens_total"`
		CostUSD     float64     `json:"cost_usd_total"`
		CostToday   float64     `json:"cost_usd_today"`
		ErrorRate   float64     `json:"error_rate"`
		P50MS       int         `json:"p50_ms"`
		P95MS       int         `json:"p95_ms"`
		P99MS       int         `json:"p99_ms"`
		TopModels   []modelStat `json:"top_models"`
		RPM60Min    []int       `json:"rpm_60min"`
		SinceTS     *time.Time  `json:"since_ts,omitempty"`
		UntilTS     *time.Time  `json:"until_ts,omitempty"`
	}{
		Total:     len(us),
		TopModels: []modelStat{},
		RPM60Min:  make([]int, 60),
	}
	if len(us) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}

	latencies := make([]int, 0, len(us))
	modelCounts := map[string]int{}
	modelCost := map[string]float64{}
	var errCount int
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for _, u := range us {
		out.TokensTotal += int64(u.PromptTokens) + int64(u.CompletionTokens)
		out.CostUSD += u.CostUSD
		if !u.TS.Before(todayStart) {
			out.CostToday += u.CostUSD
		}
		modelCounts[u.Model]++
		modelCost[u.Model] += u.CostUSD
		latencies = append(latencies, u.LatencyMS)
		switch strings.ToLower(u.Outcome) {
		case "error", "failed", "timeout", "cancelled":
			errCount++
		}
		if ago := now.Sub(u.TS); ago >= 0 && ago < 60*time.Minute {
			bucket := 59 - int(ago.Minutes())
			if bucket >= 0 && bucket < 60 {
				out.RPM60Min[bucket]++
			}
		}
	}
	sort.Ints(latencies)
	pct := func(p float64) int {
		idx := int(float64(len(latencies)) * p / 100.0)
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}
	out.P50MS = pct(50)
	out.P95MS = pct(95)
	out.P99MS = pct(99)
	out.ErrorRate = float64(errCount) / float64(len(us))

	for m, c := range modelCounts {
		out.TopModels = append(out.TopModels, modelStat{Model: m, Count: c, CostUSD: modelCost[m]})
	}
	sort.Slice(out.TopModels, func(i, j int) bool { return out.TopModels[i].Count > out.TopModels[j].Count })
	if len(out.TopModels) > 5 {
		out.TopModels = out.TopModels[:5]
	}

	// Usage rows come back newest-first; until = first row, since = last.
	since := us[len(us)-1].TS
	until := us[0].TS
	out.SinceTS = &since
	out.UntilTS = &until

	writeJSON(w, http.StatusOK, out)
}

// auditSummary aggregates the last 1000 audit entries into a compact
// "who's doing what" view: top actors, top actions, total entries.
func (s *Server) auditSummary(w http.ResponseWriter, r *http.Request) {
	es, err := s.store.Audit().Recent(r.Context(), 1000)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	type countStat struct {
		Name  string `json:"name"`
		Count int    `json:"count"`
	}
	out := struct {
		Total      int         `json:"total"`
		TopActors  []countStat `json:"top_actors"`
		TopActions []countStat `json:"top_actions"`
		SinceTS    *time.Time  `json:"since_ts,omitempty"`
		UntilTS    *time.Time  `json:"until_ts,omitempty"`
	}{
		Total:      len(es),
		TopActors:  []countStat{},
		TopActions: []countStat{},
	}
	if len(es) == 0 {
		writeJSON(w, http.StatusOK, out)
		return
	}
	actorCounts := map[string]int{}
	actionCounts := map[string]int{}
	for _, e := range es {
		actorCounts[e.Actor]++
		actionCounts[e.Action]++
	}
	for k, v := range actorCounts {
		out.TopActors = append(out.TopActors, countStat{Name: k, Count: v})
	}
	sort.Slice(out.TopActors, func(i, j int) bool { return out.TopActors[i].Count > out.TopActors[j].Count })
	if len(out.TopActors) > 5 {
		out.TopActors = out.TopActors[:5]
	}
	for k, v := range actionCounts {
		out.TopActions = append(out.TopActions, countStat{Name: k, Count: v})
	}
	sort.Slice(out.TopActions, func(i, j int) bool { return out.TopActions[i].Count > out.TopActions[j].Count })
	if len(out.TopActions) > 5 {
		out.TopActions = out.TopActions[:5]
	}
	since := es[len(es)-1].TS
	until := es[0].TS
	out.SinceTS = &since
	out.UntilTS = &until

	writeJSON(w, http.StatusOK, out)
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		s.log.Info("http",
			"method", r.Method,
			"path", r.URL.Path,
			"status", ww.Status(),
			"bytes", ww.BytesWritten(),
			"dur_ms", time.Since(start).Milliseconds(),
			"req_id", middleware.GetReqID(r.Context()),
		)
	})
}

// cacheStats returns the response cache driver + counters. Returns
// 200 with a "cache disabled" sentinel when no cache is configured
// rather than a 404 — that way the dashboard's settings tab always
// gets a parseable payload.
func (s *Server) cacheStats(w http.ResponseWriter, r *http.Request) {
	c := api.ResponseCache()
	if c == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	stats := c.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true,
		"stats":   stats,
	})
}

// cacheFlush drops cached entries. With no query string it would be
// dangerous on a busy cache; require an explicit namespace (or `all=1`
// for the nuclear option). The audit middleware records the call.
func (s *Server) cacheFlush(w http.ResponseWriter, r *http.Request) {
	c := api.ResponseCache()
	if c == nil {
		writeJSONError(w, http.StatusOK, "cache_disabled")
		return
	}
	ns := r.URL.Query().Get("namespace")
	all := r.URL.Query().Get("all") == "1"
	if ns == "" && !all {
		writeJSONError(w, http.StatusBadRequest, "specify ?namespace=<name> or ?all=1")
		return
	}
	if all {
		// Nuclear option: drop every entry regardless of namespace —
		// the memory driver resets its whole LRU, the SQLite driver
		// truncates the cache table.
		c.DeleteAll(r.Context())
	} else {
		c.DeleteNamespace(r.Context(), ns)
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "flushed", "namespace": ns, "all": all})
}

// auditMiddleware records every admin action.
//
// Target is set to the caller's remote address (useful for forensics) rather
// than the URL query string — RawQuery can contain secrets (?token=...) that
// would otherwise be persisted in plaintext to the audit_log table.
func (s *Server) auditMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r)
		actor := "anonymous"
		if k := auth.KeyFrom(r.Context()); k != nil {
			actor = k.Name
		}
		action := r.Method + " " + r.URL.Path
		// The request context is already canceled here for SSE streams
		// and client-aborted requests — detach from cancellation (but
		// keep the values) so those audit rows aren't silently dropped.
		ctx := context.WithoutCancel(r.Context())
		// Use the pre-RealIP TCP peer, not r.RemoteAddr — middleware.RealIP
		// has already rewritten the latter from spoofable X-Forwarded-For,
		// so logging it would let a caller forge the audit target.
		target := realRemoteAddr(r)
		_ = s.store.Audit().Record(ctx, store.AuditEntry{
			TS: time.Now(), Actor: actor,
			Action: action,
			Target: target,
		})

	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": "invalid_request"}})
}
