package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/opod-io/opod/internal/api"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		if !s.policy.accessLogEnabled() {
			return // per-endpoint policy: access log off (P12-2)
		}
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
