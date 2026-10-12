package leader

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// ---- shard endpoints ----

// listShards reports every shard part with its LIVE status: a part whose
// node stopped heartbeating reads "lost" (liveness.go) even though the row
// still says ready — a manager and `opod shard ls` must never see a gang as
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

// The error.code values a failed POST /admin/v1/shards/create carries (feature
// shard_create_error_codes). Part of the frozen contract: a value is never
// renamed or reused for another cause.
const (
	// ShardCreatePartLost: the gang did not form — a part's worker went away
	// while it was forming. Definite; the create will not finish on its own.
	ShardCreatePartLost = "gang_part_lost"
	// ShardCreateInFlight: refused, another create of this gang (or of the
	// whole model) is running. Not an answer about the gang; retry.
	ShardCreateInFlight = "gang_create_in_flight"
)

// shardCreateFailure names a failed create's cause the way the frozen contract
// does: the error.code (empty when the cause has none) and the reason a person
// reads. For gang_part_lost the reason is the PartLostError itself — the gang,
// the node and the last output polled — without the "upstream" prefix. The
// HTTP answer and the heal loop's shard.heal_failed event both read it here,
// so the two can never name one failure differently.
func shardCreateFailure(err error) (code, reason string) {
	switch {
	case errors.Is(err, scheduler.ErrPartLost):
		var pl *scheduler.PartLostError
		if errors.As(err, &pl) {
			return ShardCreatePartLost, pl.Error()
		}
		return ShardCreatePartLost, err.Error()
	case errors.Is(err, scheduler.ErrCreateInFlight):
		return ShardCreateInFlight, err.Error()
	}
	return "", err.Error()
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
	case errors.Is(err, scheduler.ErrUnplaceable):
		// Not an upstream fault: the fleet cannot hold the shape asked for.
		writeJSONError(w, http.StatusConflict, err.Error())
	case errors.Is(err, scheduler.ErrPartLost), errors.Is(err, scheduler.ErrCreateInFlight):
		// gang_part_lost is a definite answer: this gang did not form, its
		// parts were stopped, and asking again is a new create.
		// gang_create_in_flight is NOT an answer about the gang: another create
		// of it is running and decides. Same status as before; the code lets a
		// manager tell them apart without parsing the message (feature
		// shard_create_error_codes).
		code, msg := shardCreateFailure(err)
		writeJSONErrorCode(w, http.StatusBadGateway, code, msg)
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

// deleteGang removes ONE gang of a model. The model's other gangs keep
// serving, and the router load-balances across whatever is left.
func (s *Server) deleteGang(w http.ResponseWriter, r *http.Request) {
	modelID, gangID := chi.URLParam(r, "model_id"), chi.URLParam(r, "gang_id")
	if modelID == "" || gangID == "" {
		writeJSONError(w, http.StatusBadRequest, "model_id and gang_id required")
		return
	}
	err := s.RemoveGang(r.Context(), modelID, gangID)
	switch {
	case errors.Is(err, ErrNoOrchestrator):
		writeJSONError(w, http.StatusServiceUnavailable, err.Error())
	case err != nil && strings.Contains(err.Error(), "has no gang"):
		writeJSONError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
	default:
		writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "model_id": modelID, "gang_id": gangID})
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
