package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// ---- shard endpoints ----

// listShards reports every shard part with its LIVE status: a part whose
// node stopped heartbeating reads "lost" (liveness.go) even though the row
// still says ready — the CP and `opod shard ls` must never see a gang as
// serving when its workers are gone.
func (s *Server) listShards(w http.ResponseWriter, r *http.Request) {
	shs, err := s.store.Shards().List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	alive := s.aliveNodes(r.Context())
	for i := range shs {
		shs[i].Status = liveShardStatus(shs[i], alive)
	}
	if shs == nil {
		shs = []store.Shard{} // contract: always a JSON array, never null
	}
	writeJSON(w, http.StatusOK, shs)
}

// listShardProcesses returns the leader's supervisor view of every process
// it manages, keyed by ProcessID. The dashboard joins this against the
// shard rows from /shards to surface Restarts + the live runtime status
// (which can be running | starting | stopped | failed | crashloop). For
// shards that run on a worker (rpc-server, or a coordinator placed on a
// non-leader host), the leader doesn't see the process directly — the
// dashboard renders "—" for those.
func (s *Server) listShardProcesses(w http.ResponseWriter, r *http.Request) {
	if s.orch == nil || s.orch.Supervisor == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.orch.Supervisor.List())
}

func (s *Server) createShards(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()
	var req struct {
		ModelID string   `json:"model_id"`
		Shards  int      `json:"shards"`
		Nodes   []string `json:"nodes"` // optional: pin shards to these exact workers
		TP      int      `json:"tp"`    // optional: tensor-parallel size (vLLM only)
		PP      int      `json:"pp"`    // optional: pipeline-parallel size (vLLM only)
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	entry := models.FindByID(s.cat, req.ModelID)
	if entry == nil {
		writeJSONError(w, http.StatusNotFound, "no catalog entry for "+req.ModelID)
		return
	}
	if s.orch == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "sharding orchestrator not configured")
		return
	}
	if err := s.orch.CreateSharded(r.Context(), *entry, req.Shards, req.Nodes,
		scheduler.Parallelism{TP: req.TP, PP: req.PP}); err != nil {
		// Tear down whatever the failed create left behind, on a fresh context —
		// r.Context() is already dead when the client hung up, and that exact
		// abort used to strand "ready" rpc rows with no coordinator. A half-shard
		// wedges the next create and answers routing queries for a model that
		// cannot serve; better to converge to "not sharded" than "half sharded".
		cleanCtx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		if rmErr := s.orch.RemoveSharded(cleanCtx, req.ModelID); rmErr != nil {
			s.log.Error("shards/create cleanup failed", "model", req.ModelID, "err", rmErr)
		}
		cancel()
		s.router.InvalidateModel(req.ModelID)
		writeJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	s.router.InvalidateModel(req.ModelID)
	s.logEvent("shard.created", req.ModelID, map[string]any{"nodes": req.Nodes, "count": req.Shards})
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "model_id": req.ModelID})
}

func (s *Server) deleteShards(w http.ResponseWriter, r *http.Request) {
	modelID := chi.URLParam(r, "model_id")
	if modelID == "" {
		writeJSONError(w, http.StatusBadRequest, "model_id required")
		return
	}
	if s.orch == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "sharding orchestrator not configured")
		return
	}
	if err := s.orch.RemoveSharded(r.Context(), modelID); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.router.InvalidateModel(modelID)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "model_id": modelID})
}

// extractBearer pulls the token out of the Authorization header (Bearer
// prefix optional) or the x-api-key header.
func extractBearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h != "" {
		if len(h) > 7 && h[:7] == "Bearer " {
			return h[7:]
		}
		return h
	}
	return r.Header.Get("X-Api-Key")
}

func (s *Server) listInstalledModels(w http.ResponseWriter, r *http.Request) {
	ms, err := s.store.Models().List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ms)
}
