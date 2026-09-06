package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "ok")
}

// statusSummary returns the compact status payload used by the dashboard
// top-bar chips and `opod status --json`. Single round-trip lookup so
// polling stays cheap.
func (s *Server) statusSummary(w http.ResponseWriter, r *http.Request) {
	type engineStatus struct {
		Name      string `json:"name"`
		Endpoint  string `json:"endpoint"`
		Reachable bool   `json:"reachable"`
		Error     string `json:"error,omitempty"`
	}
	out := struct {
		Role            string       `json:"role"`
		Engine          engineStatus `json:"engine"`
		Nodes           int          `json:"nodes"`
		ModelsInstalled int          `json:"models_installed"`
	}{
		Role: "leader",
		Engine: engineStatus{
			Name:     s.engine.Name(),
			Endpoint: s.engine.Endpoint(),
		},
	}
	if err := s.engine.Health(r.Context()); err != nil {
		out.Engine.Error = err.Error()
	} else {
		out.Engine.Reachable = true
	}
	if nodes, err := s.store.Nodes().List(r.Context()); err == nil {
		out.Nodes = len(nodes)
	}
	if ms, err := s.store.Models().List(r.Context()); err == nil {
		out.ModelsInstalled = len(ms)
	}
	writeJSON(w, http.StatusOK, out)
}

// readyz answers "can this leader serve a request right now?". Two ways to be
// ready: the local engine is healthy, OR the leader is router-only (no local
// engine — the default in a cluster, see RouterConfig.PullDefaultModel) and at
// least one ALIVE worker has a ready placement (liveness.go: a worker whose
// heartbeats stopped does not count, however its rows read). A router-only
// leader with no live workers is 503 so a Kubernetes Service does not route
// to it while it cannot serve — the CP heals the workers meanwhile.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	engineErr := s.engine.Health(r.Context())
	if engineErr == nil {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "ready")
		return
	}
	if s.hasLivePlacement(r.Context()) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "mode": "router-only"})
		return
	}
	// A scale-to-zero endpoint (plan autoscale floor 0) with everything
	// parked is healthy by design: the pod must be Ready so the Deployment
	// converges; requests still get the honest 503 waking from dispatch.
	if s.plan.sleepsByDesign() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "mode": "sleeping"})
		return
	}
	// A sharded model has no placement rows — its serving unit is the
	// gang: a ready coordinator whose every part is on an alive node.
	if s.hasServableShardGroup(r.Context()) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ready", "mode": "shard-coordinator"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "degraded", "engine": engineErr.Error(), "placements": 0})
}

// hasServingCapacity reports whether a request could actually be served right
// now: a healthy local engine, an alive worker with a ready placement, or a
// servable shard gang. Same liveness rule as readyz (liveness.go) — after
// scale-to-zero or a lost node the rows linger but the workers are gone, and
// routing there would 502.
func (s *Server) hasServingCapacity(ctx context.Context) bool {
	if s.engine.Health(ctx) == nil {
		return true
	}
	return s.hasLivePlacement(ctx) || s.hasServableShardGroup(ctx)
}
