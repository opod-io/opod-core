package controlplane

// POST /admin/v1/models/{id}/move {from, to} — feature "model_move". The
// sequence is the orchestrator's (scheduler/move.go); the leader adds what
// only it knows: the catalog, the router's in-flight counts, the configured
// timeouts, and the journal — every step is an event, model.move_<step>.
//
// A move needs a live leader from start to end: it is the leader's router
// that flips and the leader's counters that say the source is idle. There is
// no store-only path, and a leader that restarts mid-move abandons it: the
// drain mark it may have set is lifted at start (liftStaleDrains), the source
// — never unloaded before the drain ended — serves again, and the copy the
// target loaded is simply a second replica.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
)

// MoveModelRequest is the move route's body. The two timeouts are optional
// (seconds; 0 = the defaults: 30 minutes for the target to serve, the
// configured placement.drain_timeout_seconds for the source to go idle).
type MoveModelRequest struct {
	From                string `json:"from"`
	To                  string `json:"to"`
	ReadyTimeoutSeconds int    `json:"ready_timeout_seconds,omitempty"`
	DrainTimeoutSeconds int    `json:"drain_timeout_seconds,omitempty"`
}

// MoveModel resolves the model and runs the move, journalling each step.
func (s *Server) MoveModel(ctx context.Context, id string, req MoveModelRequest) (scheduler.MoveResult, error) {
	if s.orch == nil {
		return scheduler.MoveResult{}, ErrNoOrchestrator
	}
	var entry *models.Entry
	if e, ok := models.ParseSchemeID(id); ok {
		entry = e
	} else {
		entry = models.FindByID(s.cat, id)
	}
	if entry == nil {
		return scheduler.MoveResult{}, ErrNoCatalogEntry
	}
	drain := time.Duration(req.DrainTimeoutSeconds) * time.Second
	if drain <= 0 {
		drain = time.Duration(s.cfg.Placement.DrainTimeoutSeconds) * time.Second
	}
	return s.orch.MoveModel(ctx, *entry, scheduler.MoveOptions{
		From: req.From, To: req.To,
		ReadyTimeout:   time.Duration(req.ReadyTimeoutSeconds) * time.Second,
		DrainTimeout:   drain,
		Settle:         s.moveSettle,
		Poll:           s.movePoll,
		Catalog:        s.cat,
		ReservePercent: s.cfg.Placement.ReservePercent,
		Inflight:       s.router.InflightByModel,
		Step: func(st scheduler.MoveStep) {
			data := map[string]any{"from": req.From, "to": req.To}
			for k, v := range st.Data {
				data[k] = v
			}
			s.record("model.move_"+st.Step, entry.ID, data)
		},
	})
}

func (s *Server) moveModel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req MoveModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.From == "" || req.To == "" {
		writeJSONError(w, http.StatusBadRequest, "from and to are required: {\"from\": \"<node>\", \"to\": \"<node>\"}")
		return
	}
	// Detached from the request on purpose: a caller that hangs up must not
	// stop a move half way (scheduler.MoveModel).
	res, err := s.MoveModel(context.WithoutCancel(r.Context()), id, req)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, res)
	case errors.Is(err, ErrNoCatalogEntry):
		writeJSONError(w, http.StatusNotFound, "no catalog entry for "+id)
	case errors.Is(err, scheduler.ErrMoveNotFound):
		writeJSONError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, scheduler.ErrUnplaceable):
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrNoOrchestrator):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	default:
		// An aborted move: the steps say how far it got and what serves now.
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"error": map[string]any{"message": err.Error(), "type": errorTypeFor(http.StatusBadGateway)},
			"steps": res.Steps,
		})
	}
}
