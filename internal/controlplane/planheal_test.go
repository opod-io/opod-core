package controlplane

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

func defaultGangIDForTest() string { return store.DefaultGangID }

// planWatcherServer is a leader with a store, watching the plan at path.
func planWatcherServer(t *testing.T, path string) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Env.PlanFile = path
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewServer(cfg, st, &downEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
}

// The rule the P6 drill wrote (design-partner cell, 2026-09-20): the
// autoscaler restored a parked gang's pods in twelve seconds with the control
// plane scaled to zero, both parts registered as ready workers, and the
// endpoint served nothing for as long as it was watched because forming a gang
// was only ever an admin call. A leader whose plan declares the gang can form
// it — but only when it is entirely absent, only once the workers are there,
// and never before a control plane that is present has had its chance.
func TestPickGangToHeal(t *testing.T) {
	now := time.Now()
	two := planGang{ID: "g0", Parts: 2, PP: 2}
	specs := []planGang{two}

	// First sight of an absent gang starts the clock and forms nothing: this
	// pass is what keeps the loop from racing a control plane that is present.
	absent := map[string]time.Time{}
	if _, _, ok := pickGangToHeal(specs, map[string]int{}, 4, absent, now); ok {
		t.Fatal("a gang must not be formed on the pass that first notices it")
	}
	if _, started := absent["g0"]; !started {
		t.Fatal("the absence clock must start on that pass")
	}

	// Still inside the window: the control plane's chance is not over.
	if _, _, ok := pickGangToHeal(specs, map[string]int{}, 4, absent, now.Add(healAfter-time.Second)); ok {
		t.Error("formed before healAfter — this would fight a working control plane")
	}

	// Past it, with the workers present: form it.
	g, id, ok := pickGangToHeal(specs, map[string]int{}, 2, absent, now.Add(healAfter+time.Second))
	if !ok {
		t.Fatal("an absent gang with enough free workers must be formed once the window passes")
	}
	if id != "g0" || g.Parts != 2 || g.PP != 2 {
		t.Errorf("formed the wrong shape: id=%q %+v", id, g)
	}

	// Not enough workers: wait, do not form a gang that cannot run.
	if _, _, ok := pickGangToHeal(specs, map[string]int{}, 1, absent, now.Add(healAfter+time.Second)); ok {
		t.Error("formed a two-part gang with one free worker")
	}

	// A gang missing ONE part is not this loop's business — re-creating it
	// would take down the part still serving.
	partial := map[string]int{"g0": 1}
	if _, _, ok := pickGangToHeal(specs, partial, 4, absent, now.Add(2*healAfter)); ok {
		t.Error("healed a gang that still holds a part: that tears down what serves")
	}
	if _, still := absent["g0"]; still {
		t.Error("a gang with parts on record must have its absence clock cleared")
	}

	// A whole gang is left alone, and its clock never starts.
	whole := map[string]int{"g0": 2}
	fresh := map[string]time.Time{}
	if _, _, ok := pickGangToHeal(specs, whole, 4, fresh, now); ok {
		t.Error("healed a gang that is already formed")
	}
	if len(fresh) != 0 {
		t.Error("a present gang must not start an absence clock")
	}
}

// One gang per pass: forming one changes who is free, so the second is decided
// on the next tick against what is actually left.
func TestHealFormsOneGangPerPass(t *testing.T) {
	now := time.Now()
	specs := []planGang{{ID: "g0", Parts: 2}, {ID: "g1", Parts: 2}}
	absent := map[string]time.Time{"g0": now.Add(-2 * healAfter), "g1": now.Add(-2 * healAfter)}

	_, id, ok := pickGangToHeal(specs, map[string]int{}, 4, absent, now)
	if !ok || id != "g0" {
		t.Fatalf("first pass must form g0, got %q ok=%v", id, ok)
	}
	// g1's clock is untouched, so the next tick forms it — and only it.
	if _, still := absent["g1"]; !still {
		t.Error("the gang not formed this pass must keep its clock")
	}
}

// The default gang id: a plan that names no gang still heals, and matches the
// rows a create without a gang name writes.
func TestHealUsesTheDefaultGangID(t *testing.T) {
	now := time.Now()
	specs := []planGang{{ID: "", Parts: 2}}
	absent := map[string]time.Time{}
	pickGangToHeal(specs, map[string]int{}, 2, absent, now)
	if _, ok := absent[defaultGangIDForTest()]; !ok {
		t.Fatalf("an unnamed gang must be tracked under the default id, got %v", absent)
	}
	// Rows stored under the default id count as that gang being present.
	if _, _, ok := pickGangToHeal(specs, map[string]int{defaultGangIDForTest(): 2}, 2, absent, now.Add(2*healAfter)); ok {
		t.Error("an unnamed gang that is already formed must be left alone")
	}
}

// The gang shape was in the mounted plan all along and the watcher read past
// it. This is the document the design-partner cell's control plane writes.
func TestPlanWatcherReadsTheGangsItDeclares(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plan.json")
	plan := `{
	  "apiVersion": "opod.io/v1",
	  "revision": 4,
	  "model": {"id": "llama-3.2-3b-sharded", "engine": "llamacpp"},
	  "shardGroups": [
	    {"id": "g0",
	     "parts": [{"node": "node-b", "gpu": 0, "vramBudgetGb": 6}, {"node": "node-c", "gpu": 0, "vramBudgetGb": 6}],
	     "parallelism": {"tensor": 1, "pipeline": 2},
	     "head": "node-b"}
	  ],
	  "autoscale": {"floor": 0, "max": 1, "unit": "gangs"}
	}`
	if err := os.WriteFile(path, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := planWatcherServer(t, path)
	srv.StartPlanWatcher(t.Context())

	specs := srv.plan.gangSpecs()
	if len(specs) != 1 {
		t.Fatalf("gangs read from the plan = %d, want 1 — the shape is in the file", len(specs))
	}
	if specs[0].ID != "g0" || specs[0].Parts != 2 || specs[0].PP != 2 || specs[0].TP != 1 {
		t.Errorf("gang read wrong: %+v", specs[0])
	}
	if rev, model := srv.plan.get(); rev != 4 || model != "llama-3.2-3b-sharded" {
		t.Errorf("the rest of the plan must still be read: rev=%d model=%q", rev, model)
	}
	if !srv.plan.sleepsByDesign() {
		t.Error("floor 0 with a max is still sleep-by-design")
	}

	// A plan with no gangs leaves nothing to heal within.
	none := filepath.Join(dir, "none.json")
	if err := os.WriteFile(none, []byte(`{"revision":1,"model":{"id":"m"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	srv2 := planWatcherServer(t, none)
	srv2.StartPlanWatcher(t.Context())
	if got := srv2.plan.gangSpecs(); len(got) != 0 {
		t.Errorf("a plan with no gangs declares none, got %v", got)
	}
}

// The loop runs on every leader, including a standalone `opod up` with no plan
// and no orchestrator. It must be a no-op there rather than a panic.
func TestHealGangsIsANoOpWithoutAPlan(t *testing.T) {
	dir := t.TempDir()
	srv := planWatcherServer(t, filepath.Join(dir, "absent.json"))
	absent := map[string]time.Time{}
	srv.healGangs(t.Context(), absent) // no plan: nothing declared
	if len(absent) != 0 {
		t.Errorf("a leader with no plan tracked a gang: %v", absent)
	}

	// A plan with gangs but no orchestrator to build them: still no panic,
	// and no clock started, because nothing could be formed anyway.
	srv.plan.mu.Lock()
	srv.plan.gangs = []planGang{{ID: "g0", Parts: 2}}
	srv.plan.modelID = "llama-3.2-3b-sharded"
	srv.plan.present = true
	srv.plan.mu.Unlock()
	srv.orch = nil
	srv.healGangs(t.Context(), absent)
	if len(absent) != 0 {
		t.Errorf("no orchestrator means nothing to heal with: %v", absent)
	}
}
