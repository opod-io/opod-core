package controlplane

// Plan as a watched file (§13 item 2, v1): in managed mode the executor
// mounts the endpoint's plan at /etc/opod/plan.json (a ConfigMap the
// kubelet syncs in place), and the leader watches it — no restart, no CP
// call. What the leader takes from it today:
//
//   - its ONE model identity (§13 item 8 / D5): gateway requests for any
//     other model are refused with a pointer to what this endpoint serves;
//   - the plan revision, surfaced on /loadz so operators and rollouts can
//     see which revision a leader actually observes.
//
// SQLite stays the rebuildable cache; auth.yaml is the next file (TARGET).

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"time"
)

const defaultPlanPath = "/etc/opod/plan.json"

type planFileState struct {
	mu        sync.RWMutex
	revision  int
	modelID   string
	present   bool
	zeroFloor bool // autoscale floor 0: all workers parked is a HEALTHY state
}

func (p *planFileState) get() (int, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.revision, p.modelID
}

// sleepsByDesign reports a mounted plan with autoscale floor 0 — zero
// serving capacity is then deliberate, not degraded.
func (p *planFileState) sleepsByDesign() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.present && p.zeroFloor
}

// StartPlanWatcher polls the plan file for changes. No file → no-op
// (standalone `opod up` has no plan and no restrictions).
func (s *Server) StartPlanWatcher(ctx context.Context) {
	path := os.Getenv("OPOD_PLAN_FILE")
	if path == "" {
		path = defaultPlanPath
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	var lastMod time.Time
	load := func() {
		st, err := os.Stat(path)
		if err != nil || !st.ModTime().After(lastMod) {
			return
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return
		}
		var doc struct {
			Revision int `json:"revision"`
			Model    struct {
				ID string `json:"id"`
			} `json:"model"`
			Autoscale struct {
				Floor int `json:"floor"`
				Max   int `json:"max"`
			} `json:"autoscale"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			s.log.Warn("plan file unreadable — keeping last good plan", "path", path, "err", err)
			return
		}
		lastMod = st.ModTime()
		s.plan.mu.Lock()
		changed := s.plan.revision != doc.Revision || s.plan.modelID != doc.Model.ID
		s.plan.revision, s.plan.modelID = doc.Revision, doc.Model.ID
		s.plan.present = true
		s.plan.zeroFloor = doc.Autoscale.Max > 0 && doc.Autoscale.Floor == 0
		s.plan.mu.Unlock()
		if changed {
			s.log.Info("plan file applied", "revision", doc.Revision, "model", doc.Model.ID)
			s.logEvent("plan.updated", doc.Model.ID, map[string]any{"revision": doc.Revision})
		}
	}
	load()
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				load()
			}
		}
	}()
}

// planAllowsModel enforces one model identity per leader when a plan file
// is present: the plan's catalog id, or any engine-native alias of it,
// passes; everything else is refused.
func (s *Server) planAllowsModel(model string) (string, bool) {
	_, planModel := s.plan.get()
	if planModel == "" {
		return "", true // no plan file — unrestricted (standalone mode)
	}
	if model == planModel || s.catalogIDForNative(model) == planModel {
		return planModel, true
	}
	return planModel, false
}
