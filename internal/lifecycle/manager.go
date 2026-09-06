// Package lifecycle implements memory-aware model placement for a
// node's local engine: admission ("does this model fit?"), LRU
// evict-and-swap ("release the least-recently-used model, then load"),
// and declarative restore ("bring back what the operator wanted loaded").
//
// Ground truth for admission is the engine's live residency report
// (Ollama /api/ps) — not the placements table, which records what a node
// *can serve* (installed models), not what occupies memory. Engines that
// can't report residency degrade to admission-only mode: the manager
// checks the incoming model against the total budget but plans no
// evictions.
//
// Evictions are deliberately soft on Ollama: an evicted model stays
// installed and reloads on the next client request for it — that's
// demand-driven loading, not a bug. The manager decides what is resident
// *now*; the engine remains the second line of defense under pressure.
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

const (
	// DefaultReservePercent of total RAM is excluded from the admission
	// budget — headroom for the OS, the engine binary, and KV growth.
	DefaultReservePercent = 20
	// DefaultDrainTimeout bounds how long an eviction waits for in-flight
	// requests before unloading anyway.
	DefaultDrainTimeout = 30 * time.Second
	// footprintOverheadDivisor: estimated RAM = weights + weights/5
	// (≈1.2×) to cover KV cache and runtime buffers for typical context
	// sizes. Conservative on purpose — refusing a borderline load beats
	// an OOM.
	footprintOverheadDivisor = 5
)

// Manager makes admission/eviction decisions for one node's local engine.
// Construct with New; all fields are then immutable except InflightFn,
// which the control plane wires after the router exists.
type Manager struct {
	Store   store.Store
	Engine  engines.Engine
	Catalog []models.Entry
	// NodeID is the placement node id this manager governs ("local").
	NodeID         string
	TotalRAMBytes  int64
	ReservePercent int
	DrainTimeout   time.Duration
	// Exclusive turns every swap into "evict ALL non-pinned residents" —
	// the one-model-per-machine policy.
	Exclusive bool
	// InflightFn returns the router's live per-(node, model) in-flight
	// counts, keyed `node_id + "|" + model` (router.InflightByModel).
	// nil skips the drain wait (e.g. `opod down`, where the server is
	// already gone).
	InflightFn func() map[string]int
	Log        *slog.Logger

	// mu serializes Load/Unload/Restore. Admission is plan-then-apply
	// against live engine residency — two concurrent loads would each
	// observe the same free space and both proceed, overcommitting the
	// node. Coarse lock is fine: these are operator-rate operations.
	mu sync.Mutex
}

// ErrNotInstalled wraps "model isn't in the models table" so HTTP
// handlers can answer 404 instead of 500.
var ErrNotInstalled = errors.New("model not installed")

// New fills defaults so callers can construct with just the required deps.
func New(st store.Store, eng engines.Engine, cat []models.Entry, totalRAMGB int, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{
		Store:          st,
		Engine:         eng,
		Catalog:        cat,
		NodeID:         "local",
		TotalRAMBytes:  int64(totalRAMGB) << 30,
		ReservePercent: DefaultReservePercent,
		DrainTimeout:   DefaultDrainTimeout,
		Log:            log,
	}
}

// Victim is one resident model the plan would evict to make room.
type Victim struct {
	CatalogID string    `json:"catalog_id"`
	Native    string    `json:"native"`
	SizeBytes int64     `json:"size_bytes"`
	LastUsed  time.Time `json:"last_used"`
	Priority  int       `json:"priority"`
}

// Plan is the admission decision for loading one model.
type Plan struct {
	ModelID         string `json:"model_id"`
	Native          string `json:"native"`
	NeedBytes       int64  `json:"need_bytes"`
	BudgetBytes     int64  `json:"budget_bytes"`
	ResidentBytes   int64  `json:"resident_bytes"`
	FreeBytes       int64  `json:"free_bytes"`
	Fits            bool   `json:"fits"`
	AlreadyResident bool   `json:"already_resident"`
	// Impossible: the model exceeds the budget even on an empty node.
	Impossible bool `json:"impossible"`
	// Degraded: the engine can't report residency (no /api/ps
	// equivalent) — admission checked against the whole budget, no
	// eviction planning.
	Degraded bool     `json:"degraded"`
	Victims  []Victim `json:"victims,omitempty"`
	// BlockedBy is set when evicting every candidate still wouldn't free
	// enough (the remainder is pinned).
	BlockedBy string `json:"blocked_by,omitempty"`
}

// NeedsSwapError is returned by Load when the model doesn't fit and the
// caller didn't opt into eviction. Carries the plan so surfaces can show
// exactly what --swap would do.
type NeedsSwapError struct{ Plan Plan }

func (e *NeedsSwapError) Error() string {
	names := make([]string, 0, len(e.Plan.Victims))
	for _, v := range e.Plan.Victims {
		names = append(names, v.CatalogID)
	}
	return fmt.Sprintf("%s needs %s but only %s is free — swap would evict: %s",
		e.Plan.ModelID, fmtBytes(e.Plan.NeedBytes), fmtBytes(e.Plan.FreeBytes), strings.Join(names, ", "))
}

// ImpossibleError reports a model that can never fit on this machine.
type ImpossibleError struct{ Plan Plan }

func (e *ImpossibleError) Error() string {
	return fmt.Sprintf("%s needs %s — over this node's %s memory budget even when empty (consider sharding or a smaller quant)",
		e.Plan.ModelID, fmtBytes(e.Plan.NeedBytes), fmtBytes(e.Plan.BudgetBytes))
}

// BlockedError reports that enough memory exists but pinned models hold it.
type BlockedError struct{ Plan Plan }

func (e *BlockedError) Error() string {
	return fmt.Sprintf("%s needs %s but pinned models hold the memory — unpin first (%s)",
		e.Plan.ModelID, fmtBytes(e.Plan.NeedBytes), e.Plan.BlockedBy)
}

// LoadOpts modifies Load behavior.
type LoadOpts struct {
	// Pin exempts the model from eviction (and from the engine's idle
	// TTL via keep_alive=-1).
	Pin bool
	// Priority orders restore (high first) and eviction (low first).
	Priority int
	// Swap authorizes evicting LRU non-pinned residents to make room.
	Swap bool
}

// resolved is the (catalog id, engine-native name, weights size) triple
// for an installed model.
type resolved struct {
	CatalogID string
	Native    string
	SizeBytes int64
}

// resolve maps a catalog id to its engine-native name + size using the
// models table (authoritative: records the exact native name used at
// install) with catalog fallback for size.
func (m *Manager) resolve(ctx context.Context, id string) (*resolved, error) {
	rec, err := m.Store.Models().Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("look up model %s: %w", id, err)
	}
	if rec == nil {
		return nil, fmt.Errorf("%w: %s — run `opod model add %s` first", ErrNotInstalled, id, id)
	}
	native := id
	if idx := strings.Index(rec.Source, ":"); idx >= 0 && idx < len(rec.Source)-1 {
		native = rec.Source[idx+1:]
	}
	size := rec.SizeBytes
	if size == 0 {
		if entry := models.FindByID(m.Catalog, id); entry != nil {
			size = entry.SizeBytes
		}
	}
	return &resolved{CatalogID: id, Native: native, SizeBytes: size}, nil
}

// footprint estimates the resident memory a model will occupy: weights
// plus ~20% overhead. When no size is known, falls back to the catalog
// hardware floor; when that's missing too, returns 0 ("unknown — admit
// optimistically", matching the install-time check's behavior for
// custom models).
func (m *Manager) footprint(ctx context.Context, id string, weightsBytes int64) int64 {
	if weightsBytes > 0 {
		return weightsBytes + weightsBytes/footprintOverheadDivisor
	}
	if entry := models.FindByID(m.Catalog, id); entry != nil && entry.Hardware.MinRAMGB > 0 {
		return int64(entry.Hardware.MinRAMGB) << 30
	}
	return 0
}

func (m *Manager) budgetBytes() int64 {
	reserve := m.ReservePercent
	if reserve <= 0 || reserve >= 100 {
		reserve = DefaultReservePercent
	}
	return m.TotalRAMBytes * int64(100-reserve) / 100
}

// nativeToCatalog builds the native-name → catalog-id map from the
// models table (Source = "<engine>:<native>"). Identity for unknowns.
func (m *Manager) nativeToCatalog(ctx context.Context) map[string]string {
	out := map[string]string{}
	rows, err := m.Store.Models().List(ctx)
	if err != nil {
		return out
	}
	for _, r := range rows {
		if idx := strings.Index(r.Source, ":"); idx >= 0 && idx < len(r.Source)-1 {
			out[r.Source[idx+1:]] = r.CatalogID
		}
	}
	return out
}

// PlanLoad computes the admission decision for loading `id` without
// applying anything.
func (m *Manager) PlanLoad(ctx context.Context, id string) (Plan, error) {
	res, err := m.resolve(ctx, id)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{
		ModelID:     res.CatalogID,
		Native:      res.Native,
		NeedBytes:   m.footprint(ctx, res.CatalogID, res.SizeBytes),
		BudgetBytes: m.budgetBytes(),
	}
	if plan.NeedBytes > plan.BudgetBytes {
		plan.Impossible = true
		return plan, nil
	}

	lister, ok := m.Engine.(engines.ResidentLister)
	if !ok {
		// Engine can't report residency — admit against the whole budget.
		plan.Degraded = true
		plan.Fits = true
		plan.FreeBytes = plan.BudgetBytes
		return plan, nil
	}
	resident, err := lister.Resident(ctx)
	if err != nil {
		return Plan{}, fmt.Errorf("query resident models: %w", err)
	}

	for _, r := range resident {
		if r.Name == res.Native {
			plan.AlreadyResident = true
			plan.Fits = true
		}
		plan.ResidentBytes += r.SizeBytes
	}
	plan.FreeBytes = plan.BudgetBytes - plan.ResidentBytes
	if plan.FreeBytes < 0 {
		plan.FreeBytes = 0
	}
	if plan.AlreadyResident {
		return plan, nil
	}
	if plan.FreeBytes >= plan.NeedBytes && !m.Exclusive {
		plan.Fits = true
		return plan, nil
	}

	// Eviction planning: rank non-pinned residents by (priority asc,
	// last-used asc) and take only as many as needed — or all of them in
	// exclusive mode.
	victims, pinnedBytes := m.rankVictims(ctx, resident, res.Native)
	needToFree := plan.NeedBytes - plan.FreeBytes
	var freed int64
	for _, v := range victims {
		if !m.Exclusive && freed >= needToFree {
			break
		}
		plan.Victims = append(plan.Victims, v)
		freed += v.SizeBytes
	}
	if plan.FreeBytes+freed >= plan.NeedBytes {
		plan.Fits = false // fits only WITH eviction; Load checks Swap
		return plan, nil
	}
	plan.Victims = nil
	plan.BlockedBy = fmt.Sprintf("%s pinned", fmtBytes(pinnedBytes))
	return plan, nil
}

// rankVictims returns eviction candidates among `resident` (excluding
// `excludeNative` and pinned models) ordered by priority asc, last-used
// asc — and the total bytes held by pinned models.
func (m *Manager) rankVictims(ctx context.Context, resident []engines.ResidentModel, excludeNative string) ([]Victim, int64) {
	n2c := m.nativeToCatalog(ctx)
	lastUsed, _ := m.Store.Usage().LastUsedByModel(ctx)
	desired, _ := m.Store.DesiredPlacements().ListByNode(ctx, m.NodeID)
	meta := map[string]store.DesiredPlacement{}
	for _, d := range desired {
		meta[d.ModelID] = d
	}

	var out []Victim
	var pinnedBytes int64
	for _, r := range resident {
		if r.Name == excludeNative {
			continue
		}
		catalogID := r.Name
		if c, ok := n2c[r.Name]; ok {
			catalogID = c
		}
		d := meta[catalogID]
		if d.Pinned {
			pinnedBytes += r.SizeBytes
			continue
		}
		// LRU timestamp: usage rows record whatever model string the
		// client sent — the catalog id normally, the native name for
		// direct engine passthrough. Check both before treating the
		// model as never-used (coldest).
		last := lastUsed[catalogID]
		if last.IsZero() {
			last = lastUsed[r.Name]
		}
		out = append(out, Victim{
			CatalogID: catalogID,
			Native:    r.Name,
			SizeBytes: r.SizeBytes,
			LastUsed:  last,
			Priority:  d.Priority,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].LastUsed.Before(out[j].LastUsed)
	})
	return out, pinnedBytes
}

// Load brings a model into memory, evicting per the plan when opts.Swap
// authorizes it. actor is recorded on audit rows (admin user id or
// "cli"). Returns the applied plan so surfaces can report what happened.
//
// Exclusive mode implies Swap: the operator already declared "loading a
// model evicts everything else" globally, so requiring --swap on every
// load would just be friction.
func (m *Manager) Load(ctx context.Context, id string, opts LoadOpts, actor string) (Plan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Exclusive {
		opts.Swap = true
	}
	plan, err := m.PlanLoad(ctx, id)
	if err != nil {
		return Plan{}, err
	}
	switch {
	case plan.Impossible:
		return plan, &ImpossibleError{Plan: plan}
	case plan.BlockedBy != "":
		return plan, &BlockedError{Plan: plan}
	case !plan.Fits && len(plan.Victims) > 0 && !opts.Swap:
		return plan, &NeedsSwapError{Plan: plan}
	}

	for _, v := range plan.Victims {
		if err := m.evict(ctx, v, plan.ModelID, actor); err != nil {
			return plan, fmt.Errorf("evict %s: %w", v.CatalogID, err)
		}
	}

	if loader, ok := m.Engine.(engines.Loader); ok {
		if err := loader.Load(ctx, plan.Native, opts.Pin); err != nil {
			return plan, fmt.Errorf("engine load %s: %w", plan.Native, err)
		}
	} else {
		m.Log.Info("engine cannot warm-load; model will load on first request",
			"engine", m.Engine.Name(), "model", plan.ModelID)
	}

	_ = m.Store.Placements().Upsert(ctx, store.Placement{
		NodeID: m.NodeID, ModelID: plan.Native, Status: "ready", LastSeen: time.Now(),
	})
	if err := m.Store.DesiredPlacements().Upsert(ctx, store.DesiredPlacement{
		NodeID: m.NodeID, ModelID: plan.ModelID, Priority: opts.Priority, Pinned: opts.Pin,
	}); err != nil {
		return plan, fmt.Errorf("persist desired placement: %w", err)
	}
	m.audit(ctx, actor, "model_loaded", plan.ModelID,
		fmt.Sprintf(`{"pin":%t,"priority":%d,"evicted":%d,"need_bytes":%d}`,
			opts.Pin, opts.Priority, len(plan.Victims), plan.NeedBytes))
	return plan, nil
}

// evict drains one victim, unloads it, and clears its desired row (the
// machine demonstrably can't hold it alongside what the operator just
// asked for — restoring it on reboot would replay the conflict).
func (m *Manager) evict(ctx context.Context, v Victim, forModel, actor string) error {
	m.drainAndSettle(ctx, v.Native, v.CatalogID)
	if err := m.Engine.Unload(ctx, v.Native); err != nil {
		// Restore routing before surfacing — the model is still resident.
		// ErrUnloadNotSupported aborts too: counting an un-unloadable
		// victim as freed (or deleting its desired row) would let the
		// subsequent load overcommit the node.
		_ = m.Store.Placements().SetStatus(ctx, m.NodeID, v.Native, "ready")
		if errors.Is(err, engines.ErrUnloadNotSupported) {
			return fmt.Errorf("engine %s cannot release %s: %w", m.Engine.Name(), v.CatalogID, err)
		}
		return err
	}
	_ = m.Store.Placements().SetStatus(ctx, m.NodeID, v.Native, "ready")
	_ = m.Store.DesiredPlacements().Delete(ctx, m.NodeID, v.CatalogID)
	m.audit(ctx, actor, "model_evicted", v.CatalogID,
		fmt.Sprintf(`{"for":"%s","bytes_freed":%d,"last_used":%d,"priority":%d}`,
			forModel, v.SizeBytes, v.LastUsed.Unix(), v.Priority))
	m.Log.Info("evicted model", "victim", v.CatalogID, "for", forModel, "bytes", v.SizeBytes)
	return nil
}

// drainAndSettle marks the placement draining (router stops routing new
// requests to it) and waits for THIS MODEL's in-flight count on this
// node to reach zero, bounded by DrainTimeout. The count is checked
// under both the catalog id and the engine-native name — requests carry
// whichever the client sent. Placement-status errors are ignored — a
// missing row just means the router never knew about the model.
func (m *Manager) drainAndSettle(ctx context.Context, native, catalogID string) {
	_ = m.Store.Placements().SetStatus(ctx, m.NodeID, native, "draining")
	if m.InflightFn == nil {
		return
	}
	keyNative := m.NodeID + "|" + native
	keyCatalog := m.NodeID + "|" + catalogID
	deadline := time.Now().Add(m.drainTimeout())
	for time.Now().Before(deadline) {
		counts := m.InflightFn()
		if counts[keyNative] == 0 && counts[keyCatalog] == 0 {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	m.Log.Warn("drain timeout — unloading with requests possibly in flight", "model", catalogID)
}

func (m *Manager) drainTimeout() time.Duration {
	if m.DrainTimeout > 0 {
		return m.DrainTimeout
	}
	return DefaultDrainTimeout
}

// Unload releases a model's memory (drain → engine unload) and clears
// its desired row so it stays unloaded across restarts.
func (m *Manager) Unload(ctx context.Context, id, actor string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	res, err := m.resolve(ctx, id)
	if err != nil {
		return err
	}
	m.drainAndSettle(ctx, res.Native, res.CatalogID)
	uerr := m.Engine.Unload(ctx, res.Native)
	_ = m.Store.Placements().SetStatus(ctx, m.NodeID, res.Native, "ready")
	_ = m.Store.DesiredPlacements().Delete(ctx, m.NodeID, res.CatalogID)
	if uerr != nil && !errors.Is(uerr, engines.ErrUnloadNotSupported) {
		return uerr
	}
	m.audit(ctx, actor, "model_unloaded", res.CatalogID, "")
	return uerr // nil or ErrUnloadNotSupported (caller renders soft warning)
}

// UnloadAll releases every resident model — `opod down`'s default. No
// drain (the server is already stopped). Returns how many were unloaded.
func (m *Manager) UnloadAll(ctx context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lister, ok := m.Engine.(engines.ResidentLister)
	if !ok {
		return 0, engines.ErrUnloadNotSupported
	}
	resident, err := lister.Resident(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	var firstErr error
	for _, r := range resident {
		if err := m.Engine.Unload(ctx, r.Name); err != nil {
			if firstErr == nil && !errors.Is(err, engines.ErrUnloadNotSupported) {
				firstErr = err
			}
			continue
		}
		n++
	}
	return n, firstErr
}

// Restore re-loads the node's desired placements in priority order —
// called in the background on `opod up` so reboots come back with the
// same models resident. Swap is allowed (desired state outranks
// whatever happens to be resident); failures log and continue so one
// missing model doesn't strand the rest.
func (m *Manager) Restore(ctx context.Context) {
	desired, err := m.Store.DesiredPlacements().ListByNode(ctx, m.NodeID)
	if err != nil || len(desired) == 0 {
		return
	}
	for _, d := range desired {
		_, err := m.Load(ctx, d.ModelID, LoadOpts{Pin: d.Pinned, Priority: d.Priority, Swap: true}, "restore")
		if err != nil {
			m.Log.Warn("restore: could not load desired model", "model", d.ModelID, "err", err)
		}
	}
}

// ResidentView is one row of the memory status (opod model ps).
type ResidentView struct {
	CatalogID string     `json:"catalog_id"`
	Native    string     `json:"native"`
	SizeBytes int64      `json:"size_bytes"`
	VRAMBytes int64      `json:"vram_bytes"`
	Pinned    bool       `json:"pinned"`
	Priority  int        `json:"priority"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

// MemoryStatus is the full memory picture for the node.
type MemoryStatus struct {
	Supported     bool                     `json:"supported"`
	Exclusive     bool                     `json:"exclusive"`
	TotalRAMBytes int64                    `json:"total_ram_bytes"`
	BudgetBytes   int64                    `json:"budget_bytes"`
	ResidentBytes int64                    `json:"resident_bytes"`
	FreeBytes     int64                    `json:"free_bytes"`
	Resident      []ResidentView           `json:"resident"`
	Desired       []store.DesiredPlacement `json:"desired"`
}

// Status reports live residency + the desired set for dashboards and
// `opod model ps`.
func (m *Manager) Status(ctx context.Context) (MemoryStatus, error) {
	out := MemoryStatus{
		Exclusive:     m.Exclusive,
		TotalRAMBytes: m.TotalRAMBytes,
		BudgetBytes:   m.budgetBytes(),
	}
	desired, err := m.Store.DesiredPlacements().ListByNode(ctx, m.NodeID)
	if err == nil {
		out.Desired = desired
	}
	lister, ok := m.Engine.(engines.ResidentLister)
	if !ok {
		return out, nil
	}
	resident, err := lister.Resident(ctx)
	if err != nil {
		return out, err
	}
	out.Supported = true
	n2c := m.nativeToCatalog(ctx)
	lastUsed, _ := m.Store.Usage().LastUsedByModel(ctx)
	meta := map[string]store.DesiredPlacement{}
	for _, d := range out.Desired {
		meta[d.ModelID] = d
	}
	for _, r := range resident {
		catalogID := r.Name
		if c, ok := n2c[r.Name]; ok {
			catalogID = c
		}
		d := meta[catalogID]
		view := ResidentView{
			CatalogID: catalogID,
			Native:    r.Name,
			SizeBytes: r.SizeBytes,
			VRAMBytes: r.VRAMBytes,
			Pinned:    d.Pinned,
			Priority:  d.Priority,
		}
		if t, ok := lastUsed[catalogID]; ok {
			view.LastUsed = &t
		}
		out.Resident = append(out.Resident, view)
		out.ResidentBytes += r.SizeBytes
	}
	out.FreeBytes = out.BudgetBytes - out.ResidentBytes
	if out.FreeBytes < 0 {
		out.FreeBytes = 0
	}
	return out, nil
}

func (m *Manager) audit(ctx context.Context, actor, action, target, metadata string) {
	if actor == "" {
		actor = "lifecycle"
	}
	_ = m.Store.Audit().Record(ctx, store.AuditEntry{
		TS: time.Now(), Actor: actor, Action: action, Target: target, Metadata: metadata,
	})
}

// fmtBytes renders a byte count as a human GB/MB string for error text.
func fmtBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MB", float64(b)/float64(1<<20))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
