package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/store"
)

func (s *Server) listNodes(w http.ResponseWriter, r *http.Request) {
	nodes, err := s.store.Nodes().List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Decorate each row: state is the LIVE state (a node whose heartbeats
	// stopped reads "lost" — liveness.go), heartbeat_age_seconds says how
	// stale it is, and cooldown_until appears when the router has the node
	// in its penalty box. JSON omits the zero cooldown so legacy clients
	// see the unchanged shape.
	type nodeView struct {
		store.Node
		State               string     `json:"state"`
		HeartbeatAgeSeconds int64      `json:"heartbeat_age_seconds"`
		CooldownUntil       *time.Time `json:"cooldown_until,omitempty"`
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	out := make([]nodeView, 0, len(nodes))
	for _, n := range nodes {
		alive := nodeAlive(n, maxAge, now)
		v := nodeView{
			Node:                n,
			State:               liveNodeState(n, alive),
			HeartbeatAgeSeconds: int64(now.Sub(n.LastHeartbeat).Seconds()),
		}
		if n.ID == "local" {
			v.HeartbeatAgeSeconds = 0
		}
		if t := s.router.CooldownUntil(n.ID); !t.IsZero() {
			v.CooldownUntil = &t
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) registerNode(w http.ResponseWriter, r *http.Request) {
	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	n, err := s.RegisterNode(r.Context(), req, callerFrom(r.Context()), extractBearer(r))
	switch {
	case errors.Is(err, ErrNodeBoundToOtherKey):
		writeJSONError(w, http.StatusForbidden, "node "+req.ID+" is bound to a different key")
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered", "id": n.ID})
}

func (s *Server) heartbeatNode(w http.ResponseWriter, r *http.Request) {
	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	err := s.HeartbeatNode(r.Context(), req, callerFrom(r.Context()))
	switch {
	case errors.Is(err, ErrUnknownNode):
		writeJSONError(w, http.StatusNotFound, "unknown node — register first")
		return
	case errors.Is(err, ErrNodeBoundToOtherKey):
		writeJSONError(w, http.StatusForbidden, "node "+req.ID+" is bound to a different key")
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- node admin ----

func (s *Server) drainNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := s.DrainNode(r.Context(), id)
	switch {
	case errors.Is(err, ErrUnknownNode):
		writeJSONError(w, http.StatusNotFound, "no such node: "+id)
		return
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "draining", "id": id})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.RemoveNode(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "id": id})
}

// PlacementSleeping marks a placement whose worker engine sleeps (sleep
// tier): the model is resident on that worker but not routable until resumed.
const PlacementSleeping = "sleeping"

// sleepWorker / resumeWorker proxy the sleep tier to a worker's agent
// (POST /admin/v1/nodes/{id}/sleep|resume → worker /v1/model/sleep|resume).
// The worker's answer is passed through: 501 "unsupported" tells the caller
// to park the pod instead.
func (s *Server) sleepWorker(w http.ResponseWriter, r *http.Request) {
	s.workerSleepCall(w, r, "sleep")
}
func (s *Server) resumeWorker(w http.ResponseWriter, r *http.Request) {
	s.workerSleepCall(w, r, "resume")
}

func (s *Server) workerSleepCall(w http.ResponseWriter, r *http.Request, action string) {
	id := chi.URLParam(r, "id")
	n, err := s.store.Nodes().Get(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == nil || n.Address == "" {
		writeJSONError(w, http.StatusNotFound, "unknown node "+id)
		return
	}
	addr := n.Address
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		addr = "http://" + addr
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(addr, "/")+"/v1/model/"+action, nil)
	req.Header.Set("Authorization", "Bearer "+n.WorkerToken)
	auth.SignRequest(req, n.ID, n.WorkerToken)
	resp, err := s.workerHTTP().Do(req)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "worker "+id+": "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK {
		s.record("worker."+action, id, map[string]any{"node": id})
		if action == "resume" {
			s.router.InvalidateModel("") // placements change on the next heartbeat; drop any cached pick
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

func (s *Server) workerHTTP() *http.Client { return &http.Client{Timeout: 2 * time.Minute} }
