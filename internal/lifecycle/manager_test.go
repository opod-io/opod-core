package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// fakeEngine scripts the residency report and records load/unload calls.
type fakeEngine struct {
	resident    []engines.ResidentModel
	residentErr error
	unloaded    []string
	loaded      []string
	pinned      map[string]bool
	noResident  bool // pretend the engine has no ResidentLister
}

func (f *fakeEngine) Name() string                           { return "fake" }
func (f *fakeEngine) Endpoint() string                       { return "fake://" }
func (f *fakeEngine) Health(context.Context) error           { return nil }
func (f *fakeEngine) List(context.Context) ([]string, error) { return nil, nil }
func (f *fakeEngine) Delete(context.Context, string) error   { return nil }
func (f *fakeEngine) Pull(context.Context, string, func(string, int64, int64)) error {
	return nil
}
func (f *fakeEngine) Chat(context.Context, engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	return nil, nil
}
func (f *fakeEngine) Unload(_ context.Context, id string) error {
	f.unloaded = append(f.unloaded, id)
	for i, r := range f.resident {
		if r.Name == id {
			f.resident = append(f.resident[:i], f.resident[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeEngine) Load(_ context.Context, id string, pin bool) error {
	f.loaded = append(f.loaded, id)
	if f.pinned == nil {
		f.pinned = map[string]bool{}
	}
	f.pinned[id] = pin
	return nil
}
func (f *fakeEngine) Resident(context.Context) ([]engines.ResidentModel, error) {
	// Return a copy: the manager iterates this while Unload mutates the
	// fake's backing slice, mirroring a real engine where /api/ps is a
	// snapshot.
	out := make([]engines.ResidentModel, len(f.resident))
	copy(out, f.resident)
	return out, f.residentErr
}

// residentLess is a bare Engine with no ResidentLister/Loader, for
// degraded-mode tests. (Must NOT embed fakeEngine — embedding would
// promote Resident/Load and the type assertions would still succeed.)
type residentLess struct{ inner *fakeEngine }

func (r residentLess) Name() string                           { return "bare" }
func (r residentLess) Endpoint() string                       { return "bare://" }
func (r residentLess) Health(context.Context) error           { return nil }
func (r residentLess) List(context.Context) ([]string, error) { return nil, nil }
func (r residentLess) Delete(context.Context, string) error   { return nil }
func (r residentLess) Pull(context.Context, string, func(string, int64, int64)) error {
	return nil
}
func (r residentLess) Chat(context.Context, engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	return nil, nil
}
func (r residentLess) Unload(context.Context, string) error {
	return engines.ErrUnloadNotSupported
}

const gib = int64(1) << 30

// newTestManager builds a Manager over a real sqlite store with three
// installed models on a 32 GB machine (budget 25.6 GB at 20% reserve):
//
//	big-old    16 GB resident, last used t=100  (coldest)
//	big-new    8 GB  resident, last used t=300
//	small-hot  1 GB  resident, last used t=200
func newTestManager(t *testing.T) (*Manager, *fakeEngine, store.Store) {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()

	install := func(id, native string, size int64) {
		if err := st.Models().Upsert(ctx, store.Model{
			ID: id, CatalogID: id, Source: "ollama:" + native, Status: "ready",
			SizeBytes: size, InstalledAt: time.Unix(1, 0),
		}); err != nil {
			t.Fatalf("install %s: %v", id, err)
		}
		if err := st.Placements().Upsert(ctx, store.Placement{
			NodeID: "local", ModelID: native, Status: "ready", LastSeen: time.Now(),
		}); err != nil {
			t.Fatalf("place %s: %v", id, err)
		}
	}
	install("big-old", "big-old:latest", 16*gib)
	install("big-new", "big-new:latest", 8*gib)
	install("small-hot", "small-hot:latest", 1*gib)
	install("incoming-9g", "incoming-9g:latest", 9*gib)
	install("incoming-2g", "incoming-2g:latest", 2*gib)
	install("incoming-60g", "incoming-60g:latest", 60*gib)

	use := func(model string, ts int64) {
		if err := st.Usage().Record(ctx, store.Usage{
			TS: time.Unix(ts, 0), APIKeyID: "k", UserID: "u", Model: model,
			Protocol: "openai", Outcome: "ok",
		}); err != nil {
			t.Fatalf("usage %s: %v", model, err)
		}
	}
	use("big-old", 100)
	use("small-hot", 200)
	use("big-new", 300)

	eng := &fakeEngine{resident: []engines.ResidentModel{
		{Name: "big-old:latest", SizeBytes: 16 * gib},
		{Name: "big-new:latest", SizeBytes: 8 * gib},
		{Name: "small-hot:latest", SizeBytes: 1 * gib},
	}}
	var cat []models.Entry
	m := New(st, eng, cat, 32, slog.Default())
	return m, eng, st
}

func TestPlanLoadFits(t *testing.T) {
	m, _, _ := newTestManager(t)
	// budget 25.6G, resident 25G → free 0.6G; incoming-2g needs 2.4G → doesn't fit.
	// First check something that DOES fit: drop resident to make room.
	m.Engine.(*fakeEngine).resident = m.Engine.(*fakeEngine).resident[:1] // 16G resident, 9.6G free
	plan, err := m.PlanLoad(context.Background(), "incoming-2g")
	if err != nil {
		t.Fatalf("PlanLoad: %v", err)
	}
	if !plan.Fits || len(plan.Victims) != 0 {
		t.Errorf("want clean fit, got %+v", plan)
	}
}

func TestPlanLoadVictimsAreLRUAndMinimal(t *testing.T) {
	m, _, _ := newTestManager(t)
	// incoming-9g needs 10.8G; free is 0.6G → must free ~10.2G.
	// LRU order: big-old (t=100) first — 16G alone is enough. small-hot
	// (t=200) and big-new (t=300) must NOT be evicted.
	plan, err := m.PlanLoad(context.Background(), "incoming-9g")
	if err != nil {
		t.Fatalf("PlanLoad: %v", err)
	}
	if plan.Fits || plan.Impossible || plan.BlockedBy != "" {
		t.Fatalf("want needs-swap plan, got %+v", plan)
	}
	if len(plan.Victims) != 1 || plan.Victims[0].CatalogID != "big-old" {
		t.Errorf("victims = %+v, want exactly [big-old] (LRU, minimal)", plan.Victims)
	}
}

func TestPlanLoadPinnedBlocksEviction(t *testing.T) {
	m, _, st := newTestManager(t)
	ctx := context.Background()
	for _, id := range []string{"big-old", "big-new", "small-hot"} {
		if err := st.DesiredPlacements().Upsert(ctx, store.DesiredPlacement{
			NodeID: "local", ModelID: id, Pinned: true,
		}); err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
	}
	plan, err := m.PlanLoad(ctx, "incoming-9g")
	if err != nil {
		t.Fatalf("PlanLoad: %v", err)
	}
	if plan.BlockedBy == "" || len(plan.Victims) != 0 {
		t.Errorf("want blocked-by-pinned plan, got %+v", plan)
	}
	if _, err := m.Load(ctx, "incoming-9g", LoadOpts{Swap: true}, "t"); err == nil {
		t.Errorf("Load over pinned memory: want BlockedError, got nil")
	} else {
		var be *BlockedError
		if !errors.As(err, &be) {
			t.Errorf("want BlockedError, got %T: %v", err, err)
		}
	}
}

func TestPlanLoadImpossible(t *testing.T) {
	m, _, _ := newTestManager(t)
	plan, err := m.PlanLoad(context.Background(), "incoming-60g")
	if err != nil {
		t.Fatalf("PlanLoad: %v", err)
	}
	if !plan.Impossible {
		t.Errorf("60G model on 32G machine: want Impossible, got %+v", plan)
	}
	if _, err := m.Load(context.Background(), "incoming-60g", LoadOpts{Swap: true}, "t"); err == nil {
		t.Errorf("want ImpossibleError, got nil")
	}
}

func TestLoadRequiresSwapThenEvicts(t *testing.T) {
	m, eng, st := newTestManager(t)
	ctx := context.Background()

	// Without --swap: typed refusal, nothing unloaded.
	_, err := m.Load(ctx, "incoming-9g", LoadOpts{}, "alice")
	var ns *NeedsSwapError
	if !errors.As(err, &ns) {
		t.Fatalf("want NeedsSwapError, got %T: %v", err, err)
	}
	if len(eng.unloaded) != 0 {
		t.Fatalf("refusal must not unload anything, unloaded=%v", eng.unloaded)
	}

	// With --swap: big-old evicted, model loaded + desired row written,
	// victim's desired row removed, audit trail present.
	if err := st.DesiredPlacements().Upsert(ctx, store.DesiredPlacement{
		NodeID: "local", ModelID: "big-old",
	}); err != nil {
		t.Fatal(err)
	}
	plan, err := m.Load(ctx, "incoming-9g", LoadOpts{Swap: true, Pin: true, Priority: 7}, "alice")
	if err != nil {
		t.Fatalf("Load --swap: %v", err)
	}
	if len(eng.unloaded) != 1 || eng.unloaded[0] != "big-old:latest" {
		t.Errorf("unloaded = %v, want [big-old:latest]", eng.unloaded)
	}
	if len(eng.loaded) != 1 || eng.loaded[0] != "incoming-9g:latest" || !eng.pinned["incoming-9g:latest"] {
		t.Errorf("loaded = %v pinned=%v, want warm pinned load of incoming-9g:latest", eng.loaded, eng.pinned)
	}
	if len(plan.Victims) != 1 {
		t.Errorf("plan victims = %+v", plan.Victims)
	}
	d, _ := st.DesiredPlacements().Get(ctx, "local", "incoming-9g")
	if d == nil || !d.Pinned || d.Priority != 7 {
		t.Errorf("desired row = %+v, want pinned prio 7", d)
	}
	if v, _ := st.DesiredPlacements().Get(ctx, "local", "big-old"); v != nil {
		t.Errorf("victim's desired row should be cleared, got %+v", v)
	}
	audits, _ := st.Audit().Recent(ctx, 10)
	var sawEvict, sawLoad bool
	for _, a := range audits {
		if a.Action == "model_evicted" && a.Target == "big-old" && a.Actor == "alice" {
			sawEvict = true
		}
		if a.Action == "model_loaded" && a.Target == "incoming-9g" {
			sawLoad = true
		}
	}
	if !sawEvict || !sawLoad {
		t.Errorf("audit trail incomplete: evict=%v load=%v (%+v)", sawEvict, sawLoad, audits)
	}
}

func TestExclusiveEvictsAllNonPinned(t *testing.T) {
	m, eng, st := newTestManager(t)
	m.Exclusive = true
	ctx := context.Background()
	if err := st.DesiredPlacements().Upsert(ctx, store.DesiredPlacement{
		NodeID: "local", ModelID: "small-hot", Pinned: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Load(ctx, "incoming-2g", LoadOpts{Swap: true}, "t"); err != nil {
		t.Fatalf("Load exclusive: %v", err)
	}
	// All non-pinned residents go, pinned small-hot stays.
	if len(eng.unloaded) != 2 {
		t.Errorf("exclusive should evict both big models, unloaded=%v", eng.unloaded)
	}
	for _, u := range eng.unloaded {
		if u == "small-hot:latest" {
			t.Errorf("pinned model evicted in exclusive mode")
		}
	}
}

func TestAlreadyResidentShortCircuits(t *testing.T) {
	m, eng, _ := newTestManager(t)
	plan, err := m.Load(context.Background(), "big-new", LoadOpts{}, "t")
	if err != nil {
		t.Fatalf("Load resident model: %v", err)
	}
	if !plan.AlreadyResident {
		t.Errorf("want AlreadyResident, got %+v", plan)
	}
	if len(eng.unloaded) != 0 {
		t.Errorf("no eviction expected, unloaded=%v", eng.unloaded)
	}
}

func TestDegradedModeWithoutResidentLister(t *testing.T) {
	m, eng, _ := newTestManager(t)
	m.Engine = residentLess{inner: eng}
	plan, err := m.PlanLoad(context.Background(), "incoming-9g")
	if err != nil {
		t.Fatalf("PlanLoad degraded: %v", err)
	}
	if !plan.Degraded || !plan.Fits {
		t.Errorf("degraded engines admit against full budget, got %+v", plan)
	}
	// Still impossible when over the whole budget.
	plan, err = m.PlanLoad(context.Background(), "incoming-60g")
	if err != nil {
		t.Fatalf("PlanLoad degraded 60g: %v", err)
	}
	if !plan.Impossible {
		t.Errorf("degraded mode must still refuse over-budget models, got %+v", plan)
	}
}

func TestUnloadClearsDesiredRow(t *testing.T) {
	m, eng, st := newTestManager(t)
	ctx := context.Background()
	if err := st.DesiredPlacements().Upsert(ctx, store.DesiredPlacement{
		NodeID: "local", ModelID: "big-new", Pinned: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := m.Unload(ctx, "big-new", "t"); err != nil {
		t.Fatalf("Unload: %v", err)
	}
	if len(eng.unloaded) != 1 || eng.unloaded[0] != "big-new:latest" {
		t.Errorf("unloaded = %v", eng.unloaded)
	}
	if d, _ := st.DesiredPlacements().Get(ctx, "local", "big-new"); d != nil {
		t.Errorf("desired row should be gone after unload, got %+v", d)
	}
}

func TestUnloadAll(t *testing.T) {
	m, eng, _ := newTestManager(t)
	n, err := m.UnloadAll(context.Background())
	if err != nil {
		t.Fatalf("UnloadAll: %v", err)
	}
	if n != 3 || len(eng.resident) != 0 {
		t.Errorf("UnloadAll: n=%d resident=%v, want 3 and empty", n, eng.resident)
	}
}

func TestDrainWaitsForVictimInflightOnly(t *testing.T) {
	m, _, _ := newTestManager(t)
	m.DrainTimeout = 2 * time.Second
	calls := 0
	m.InflightFn = func() map[string]int {
		calls++
		if calls < 3 {
			// Victim still has traffic under its catalog id.
			return map[string]int{"local|big-old": 2, "local|small-hot": 5}
		}
		// Victim drained; OTHER models' traffic must not block the drain.
		return map[string]int{"local|small-hot": 5}
	}
	start := time.Now()
	m.drainAndSettle(context.Background(), "big-old:latest", "big-old")
	if calls < 3 {
		t.Errorf("drain should poll until the victim is idle, polled %d times", calls)
	}
	if time.Since(start) > time.Second {
		t.Errorf("drain blocked on unrelated models' traffic: settled after %v", time.Since(start))
	}
}

func TestExclusiveImpliesSwap(t *testing.T) {
	m, eng, _ := newTestManager(t)
	m.Exclusive = true
	// No Swap passed — exclusive mode authorizes eviction by itself.
	if _, err := m.Load(context.Background(), "incoming-2g", LoadOpts{}, "t"); err != nil {
		t.Fatalf("exclusive load without --swap should succeed, got %v", err)
	}
	if len(eng.unloaded) == 0 {
		t.Errorf("exclusive load should have evicted the other residents")
	}
}

// unloadRefuser reports residency but cannot release memory — the
// combination where a swap must ABORT rather than count the victim as
// freed and overcommit.
type unloadRefuser struct{ *fakeEngine }

func (u unloadRefuser) Unload(context.Context, string) error {
	return engines.ErrUnloadNotSupported
}

func TestEvictAbortsWhenEngineCannotUnload(t *testing.T) {
	m, eng, st := newTestManager(t)
	m.Engine = unloadRefuser{eng}
	ctx := context.Background()
	if err := st.DesiredPlacements().Upsert(ctx, store.DesiredPlacement{
		NodeID: "local", ModelID: "big-old",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.Load(ctx, "incoming-9g", LoadOpts{Swap: true}, "t")
	if err == nil {
		t.Fatalf("swap on an engine that can't unload must fail, got nil")
	}
	// The victim's desired row must survive an aborted eviction.
	if d, _ := st.DesiredPlacements().Get(ctx, "local", "big-old"); d == nil {
		t.Errorf("aborted eviction deleted the victim's desired row")
	}
	// And the new model must NOT have been warm-loaded on top.
	if len(eng.loaded) != 0 {
		t.Errorf("aborted swap still loaded the new model: %v", eng.loaded)
	}
}

func TestRestoreLoadsDesiredInPriorityOrder(t *testing.T) {
	m, eng, st := newTestManager(t)
	ctx := context.Background()
	// Empty the machine so restore has room and order is observable.
	eng.resident = nil
	for _, d := range []store.DesiredPlacement{
		{NodeID: "local", ModelID: "incoming-2g", Priority: 1, CreatedAt: time.Unix(10, 0)},
		{NodeID: "local", ModelID: "small-hot", Priority: 9, Pinned: true, CreatedAt: time.Unix(20, 0)},
	} {
		if err := st.DesiredPlacements().Upsert(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	m.Restore(ctx)
	if len(eng.loaded) != 2 || eng.loaded[0] != "small-hot:latest" || eng.loaded[1] != "incoming-2g:latest" {
		t.Errorf("restore order = %v, want [small-hot:latest incoming-2g:latest]", eng.loaded)
	}
	if !eng.pinned["small-hot:latest"] {
		t.Errorf("restore must preserve pin")
	}
}
