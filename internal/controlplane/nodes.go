package controlplane

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/events"
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
	var req struct {
		ID           string `json:"id"`
		Hostname     string `json:"hostname"`
		OS           string `json:"os"`
		Arch         string `json:"arch"`
		RAMGB        int    `json:"ram_gb"`
		Address      string `json:"address"`
		HardwareJSON string `json:"hardware_json"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	// First-use binding: the key that first registers a node id owns it.
	// A node-scope key presenting a different id is refused, so one
	// leaked node token can't impersonate every node. Admin keys always
	// pass; legacy rows (no binding) bind on this register.
	key := auth.KeyFrom(r.Context())
	existing, err := s.store.Nodes().Get(r.Context(), req.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	boundKeyID := ""
	if key != nil {
		boundKeyID = key.ID
	}
	if existing != nil && existing.BoundKeyID != "" {
		if auth.ScopeFrom(r.Context()) != "admin" && (key == nil || key.ID != existing.BoundKeyID) {
			writeJSONError(w, http.StatusForbidden, "node "+req.ID+" is bound to a different key")
			return
		}
		// Keep the original binding — an admin re-register shouldn't
		// silently re-own the node.
		boundKeyID = existing.BoundKeyID
	}
	// The presented bearer token doubles as the shared secret for both
	// directions of communication. Store it on the node row so the router
	// can authenticate outbound calls to the worker.
	// NOTE: stored plaintext today — assumes a trusted network (LAN or
	// Tailscale). Replace with HMAC-based mutual auth once the OIDC +
	// key-management story lands.
	workerToken := extractBearer(r)
	n := store.Node{
		ID:            req.ID,
		Hostname:      req.Hostname,
		OS:            req.OS,
		Arch:          req.Arch,
		RAMGB:         req.RAMGB,
		Address:       req.Address,
		WorkerToken:   workerToken,
		BoundKeyID:    boundKeyID,
		HardwareJSON:  req.HardwareJSON,
		LastHeartbeat: time.Now(),
		State:         "ready",
	}
	if err := s.store.Nodes().Upsert(r.Context(), n); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.bus.Publish(events.Event{Topic: events.TopicNodes, ID: n.ID})
	s.logEvent("node.registered", n.ID, map[string]any{"hostname": n.Hostname, "address": n.Address})
	writeJSON(w, http.StatusOK, map[string]string{"status": "registered", "id": n.ID})
}

func (s *Server) heartbeatNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID           string   `json:"id"`
		LoadedModels []string `json:"loaded_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid body")
		return
	}
	n, err := s.store.Nodes().Get(r.Context(), req.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == nil {
		writeJSONError(w, http.StatusNotFound, "unknown node — register first")
		return
	}
	// Enforce the register-time key binding. Admin keys always pass;
	// unbound legacy rows pass too (they bind on their next register).
	if n.BoundKeyID != "" && auth.ScopeFrom(r.Context()) != "admin" {
		if key := auth.KeyFrom(r.Context()); key == nil || key.ID != n.BoundKeyID {
			writeJSONError(w, http.StatusForbidden, "node "+req.ID+" is bound to a different key")
			return
		}
	}
	n.LastHeartbeat = time.Now()
	if n.State == "joining" {
		n.State = "ready"
	}
	if err := s.store.Nodes().Upsert(r.Context(), *n); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Reconcile placements with what the worker reports loaded right now.
	// Workers report the ENGINE-NATIVE name (e.g. vLLM's HF repo
	// "XiaomiMiMo/MiMo-7B-RL"); map it back to the catalog id ("mimo-7b") so the
	// router matches a request for the catalog id to this placement instead of
	// falling back to the leader-local engine.
	// Dedupe after mapping: an engine may list one model under several names
	// (vLLM serves both the catalog id and the native repo name) that all
	// resolve to the same catalog id — one placement row per (node, model).
	placements := make([]store.Placement, 0, len(req.LoadedModels))
	seen := make(map[string]bool, len(req.LoadedModels))
	for _, m := range req.LoadedModels {
		id := s.catalogIDForNative(m)
		if seen[id] {
			continue
		}
		seen[id] = true
		placements = append(placements, store.Placement{
			NodeID:   req.ID,
			ModelID:  id,
			Status:   "ready",
			LastSeen: time.Now(),
		})
	}
	// Diff before/after so the event log records model residency changes.
	prev := map[string]bool{}
	if old, err := s.store.Placements().GetByNode(r.Context(), req.ID); err == nil {
		for _, p := range old {
			prev[p.ModelID] = true
		}
	}
	if err := s.store.Placements().ReplaceForNode(r.Context(), req.ID, placements); err != nil {
		s.log.Warn("placements replace failed", "node", req.ID, "err", err)
	} else {
		cur := map[string]bool{}
		for _, p := range placements {
			cur[p.ModelID] = true
			if !prev[p.ModelID] {
				s.logEvent("model.loaded", p.ModelID, map[string]any{"node": req.ID})
			}
		}
		for m := range prev {
			if !cur[m] {
				s.logEvent("model.unloaded", m, map[string]any{"node": req.ID})
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ---- node admin ----

func (s *Server) drainNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	n, err := s.store.Nodes().Get(r.Context(), id)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n == nil {
		writeJSONError(w, http.StatusNotFound, "no such node: "+id)
		return
	}
	n.State = "draining"
	if err := s.store.Nodes().Upsert(r.Context(), *n); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.bus.Publish(events.Event{Topic: events.TopicNodes, ID: id})
	s.logEvent("node.drained", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "draining", "id": id})
}

func (s *Server) deleteNode(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if err := s.store.Nodes().Delete(r.Context(), id); err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Also clean up placements for the removed node so the router doesn't
	// keep trying it.
	if ps, _ := s.store.Placements().GetByNode(r.Context(), id); ps != nil {
		for _, p := range ps {
			_ = s.store.Placements().Delete(r.Context(), p.NodeID, p.ModelID)
		}
	}
	// Drop the router's cached remote engine for this node so in-flight
	// routing stops picking the removed worker immediately.
	s.router.InvalidateNode(id)
	s.bus.Publish(events.Event{Topic: events.TopicNodes, ID: id})
	s.logEvent("node.removed", id, nil)
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed", "id": id})
}
