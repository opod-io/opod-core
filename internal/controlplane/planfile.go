package controlplane

// Plan as a watched file (§13 item 2, v1): in managed mode the executor
// mounts the endpoint's plan at /etc/opod/plan.json (a ConfigMap the
// kubelet syncs in place), and the leader watches it — no restart, no CP
// call. What the leader takes from it today:
//
//   - its ONE model identity (§13 item 8 / D5): gateway requests for any
//     other model are refused with a pointer to what this endpoint serves;
//   - the plan revision, surfaced on /loadz so operators and rollouts can
//     see which revision a leader actually observes;
//   - the ADAPTER NAMES the plan declares (R15.15). "<model>:<name>" used to
//     pass on its shape alone, so a typo reached the router, found no
//     placement and came back as a routing failure. The plan knows the set, so
//     an unknown suffix is refused here by name, listing what does exist.
//
// SQLite stays the rebuildable cache; auth.yaml is the next file (TARGET).

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

const defaultPlanPath = "/etc/opod/plan.json"

type planFileState struct {
	mu        sync.RWMutex
	revision  int
	modelID   string
	adapters  []string // names the plan declares; empty = the plan states none
	present   bool
	zeroFloor bool // autoscale floor 0: all workers parked is a HEALTHY state
}

func (p *planFileState) get() (int, string) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.revision, p.modelID
}

// adapterNames is the LoRA set the plan declares, copied out under the lock.
func (p *planFileState) adapterNames() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.adapters...)
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
	path := s.cfg.Env.PlanFile
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
			Adapters []struct {
				Name string `json:"name"`
			} `json:"adapters"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			s.log.Warn("plan file unreadable — keeping last good plan", "path", path, "err", err)
			return
		}
		lastMod = st.ModTime()
		s.plan.mu.Lock()
		changed := s.plan.revision != doc.Revision || s.plan.modelID != doc.Model.ID
		names := make([]string, 0, len(doc.Adapters))
		for _, a := range doc.Adapters {
			if a.Name != "" {
				names = append(names, a.Name)
			}
		}
		s.plan.revision, s.plan.modelID, s.plan.adapters = doc.Revision, doc.Model.ID, names
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
	// A LoRA adapter is a variant of the one identity (feature "lora"):
	// "<model>:<adapter>" is served by the workers holding the base. The plan
	// names the set, so a suffix it does not name is refused HERE — before the
	// router turns a typo into "no placement for this model".
	if strings.HasPrefix(model, planModel+":") && len(model) > len(planModel)+1 {
		want := model[len(planModel)+1:]
		names := s.plan.adapterNames()
		if len(names) == 0 {
			// The plan declares no adapters. It may still be an older plan
			// document that never carried the field, so this is not the place
			// to refuse: the worker that holds the adapter answers, or the
			// router says it has no placement.
			return planModel, true
		}
		for _, n := range names {
			if n == want {
				return planModel, true
			}
		}
		return planModel + " with adapters " + strings.Join(names, ", "), false
	}
	return planModel, false
}
