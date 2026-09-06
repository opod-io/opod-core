package controlplane

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/events"
	"github.com/opod-io/opod/internal/lifecycle"
)

// AttachLifecycle wires the memory-lifecycle manager into the server:
// the manager gets the router's live per-(node, model) in-flight counts
// (so eviction drains wait only on the victim's traffic) and the admin
// surface gains /models/{id}/load + /memory. Called from cmd_up after
// both exist; nil-safe handlers below return 503 when the manager was
// never attached (e.g. older embedded uses).
func (s *Server) AttachLifecycle(m *lifecycle.Manager) {
	m.InflightFn = s.router.InflightByModel
	s.lifecycle = m
}

// loadModel brings a model into engine memory with admission control.
//
//	POST /admin/v1/models/{id}/load
//	  { "swap": false, "pin": false, "priority": 0 }
//
// Responses:
//
//	200 {status, plan}            — loaded (possibly after evictions)
//	409 type=needs_swap           — doesn't fit; body carries the victim
//	                                list; retry with swap:true to accept
//	409 type=blocked_by_pinned    — pinned models hold the memory
//	422 type=impossible           — exceeds the node's budget when empty
func (s *Server) loadModel(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "lifecycle manager not configured")
		return
	}
	defer r.Body.Close()
	id := chi.URLParam(r, "id")
	var req struct {
		Swap     bool `json:"swap"`
		Pin      bool `json:"pin"`
		Priority int  `json:"priority"`
	}
	// Empty body = defaults; malformed body is still a 400.
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}

	plan, err := s.lifecycle.Load(r.Context(), id,
		lifecycle.LoadOpts{Swap: req.Swap, Pin: req.Pin, Priority: req.Priority},
		actorFrom(r))
	if err != nil {
		var needsSwap *lifecycle.NeedsSwapError
		var blocked *lifecycle.BlockedError
		var impossible *lifecycle.ImpossibleError
		switch {
		case errors.Is(err, lifecycle.ErrNotInstalled):
			writeJSONError(w, http.StatusNotFound, err.Error())
		case errors.As(err, &needsSwap):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]any{"type": "needs_swap", "message": err.Error()},
				"plan":  plan,
			})
		case errors.As(err, &blocked):
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]any{"type": "blocked_by_pinned", "message": err.Error()},
				"plan":  plan,
			})
		case errors.As(err, &impossible):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
				"error": map[string]any{"type": "impossible", "message": err.Error()},
				"plan":  plan,
			})
		default:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	s.bus.Publish(events.Event{Topic: events.TopicModels, ID: id})
	writeJSON(w, http.StatusOK, map[string]any{"status": "loaded", "id": id, "plan": plan})
}

// memoryStatus reports live engine residency + the desired set.
//
//	GET /admin/v1/memory
func (s *Server) memoryStatus(w http.ResponseWriter, r *http.Request) {
	if s.lifecycle == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "lifecycle manager not configured")
		return
	}
	st, err := s.lifecycle.Status(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, st)
}

// actorFrom names the authenticated caller for audit rows.
func actorFrom(r *http.Request) string {
	if k := auth.KeyFrom(r.Context()); k != nil {
		if k.UserID != "" {
			return k.UserID
		}
		return k.Name
	}
	return "anonymous"
}
