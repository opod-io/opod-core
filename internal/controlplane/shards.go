package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
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
	var req CreateShardsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	err := s.CreateShards(r.Context(), req)
	switch {
	case errors.Is(err, ErrNoCatalogEntry):
		writeJSONError(w, http.StatusNotFound, "no catalog entry for "+req.ModelID)
	case errors.Is(err, ErrNoOrchestrator):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeJSONError(w, http.StatusBadGateway, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "model_id": req.ModelID})
	}
}

func (s *Server) deleteShards(w http.ResponseWriter, r *http.Request) {
	modelID := chi.URLParam(r, "model_id")
	if modelID == "" {
		writeJSONError(w, http.StatusBadRequest, "model_id required")
		return
	}
	err := s.RemoveShards(r.Context(), modelID)
	switch {
	case errors.Is(err, ErrNoOrchestrator):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "model_id": modelID})
	}
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
