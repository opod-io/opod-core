package store

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// TestAPIKeyAllowedModelsRoundtrip verifies the three states of the
// allowed_models column round-trip correctly: nil ("any model"), empty
// ([]string{}, "deny all"), and an explicit list. Caught two latent
// JSON-decode bugs in early drafts; keep the explicit cases.
func TestAPIKeyAllowedModelsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSQLite(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	cases := []struct {
		name string
		list []string
	}{
		{"nil → unrestricted", nil},
		{"empty → deny all", []string{}},
		{"single literal", []string{"qwen3-14b"}},
		{"multiple + wildcards", []string{"qwen-coder-7b", "claude-*", "gpt-4o-mini"}},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id := "k_test_" + c.name
			rec := APIKey{
				ID:            id,
				Hash:          "hash_" + c.name,
				Name:          c.name,
				Scope:         "user",
				UserID:        "alice",
				AllowedModels: c.list,
				CreatedAt:     time.Unix(int64(1_700_000_000+i), 0),
			}
			if err := st.APIKeys().Create(ctx, rec); err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := st.APIKeys().GetByID(ctx, id)
			if err != nil {
				t.Fatalf("GetByID: %v", err)
			}
			if got == nil {
				t.Fatalf("GetByID: got nil")
			}
			if !sameSlice(got.AllowedModels, c.list) {
				t.Errorf("AllowedModels round-trip: got %#v (nil=%v) want %#v (nil=%v)",
					got.AllowedModels, got.AllowedModels == nil, c.list, c.list == nil)
			}

			// And via UpdateAllowedModels.
			if err := st.APIKeys().UpdateAllowedModels(ctx, id, []string{"updated"}); err != nil {
				t.Fatalf("UpdateAllowedModels: %v", err)
			}
			got, _ = st.APIKeys().GetByID(ctx, id)
			if !reflect.DeepEqual(got.AllowedModels, []string{"updated"}) {
				t.Errorf("after Update: %#v", got.AllowedModels)
			}
			// Clear back to nil.
			if err := st.APIKeys().UpdateAllowedModels(ctx, id, nil); err != nil {
				t.Fatalf("UpdateAllowedModels nil: %v", err)
			}
			got, _ = st.APIKeys().GetByID(ctx, id)
			if got.AllowedModels != nil {
				t.Errorf("after clear: got %#v want nil", got.AllowedModels)
			}
		})
	}
}

// TestUsageBreakdown_ByDayAndModel writes a few synthetic usage rows
// then verifies the bucketed query rolls them up correctly.
func TestUsageBreakdown_ByDayAndModel(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSQLite(filepath.Join(dir, "u.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// Two rows on 2026-06-08 for alice/qwen, two for bob/claude on
	// 2026-06-09, and one alice/qwen on 2026-06-09.
	rows := []Usage{
		{TS: mustDay(t, "2026-06-08T10:00:00Z"), UserID: "alice", Model: "qwen3-14b", PromptTokens: 100, CompletionTokens: 50, Protocol: "openai", Outcome: "ok"},
		{TS: mustDay(t, "2026-06-08T11:00:00Z"), UserID: "alice", Model: "qwen3-14b", PromptTokens: 200, CompletionTokens: 100, Protocol: "openai", Outcome: "ok"},
		{TS: mustDay(t, "2026-06-09T09:00:00Z"), UserID: "bob", Model: "claude-3-5-sonnet", PromptTokens: 80, CompletionTokens: 40, Protocol: "anthropic", Outcome: "ok"},
		{TS: mustDay(t, "2026-06-09T10:00:00Z"), UserID: "bob", Model: "claude-3-5-sonnet", PromptTokens: 80, CompletionTokens: 40, Protocol: "anthropic", Outcome: "error"},
		{TS: mustDay(t, "2026-06-09T11:00:00Z"), UserID: "alice", Model: "qwen3-14b", PromptTokens: 50, CompletionTokens: 25, Protocol: "openai", Outcome: "ok"},
	}
	for _, r := range rows {
		if err := st.Usage().Record(ctx, r); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}

	got, totals, err := st.Usage().Breakdown(ctx, BreakdownOpts{
		Bucket:  "day",
		Since:   mustDay(t, "2026-06-08T00:00:00Z"),
		Until:   mustDay(t, "2026-06-10T00:00:00Z"),
		GroupBy: []string{"user", "model"},
	})
	if err != nil {
		t.Fatalf("Breakdown: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 groups (alice+qwen on 08, bob+claude on 09, alice+qwen on 09), got %d: %+v", len(got), got)
	}
	if totals.Requests != 5 {
		t.Errorf("totals.Requests = %d, want 5", totals.Requests)
	}
	if totals.PromptTokens != 510 {
		t.Errorf("totals.PromptTokens = %d, want 510", totals.PromptTokens)
	}

	// totals mode rolls everything into one bucket.
	tot, _, err := st.Usage().Breakdown(ctx, BreakdownOpts{
		Bucket:  "total",
		Since:   mustDay(t, "2026-06-08T00:00:00Z"),
		Until:   mustDay(t, "2026-06-10T00:00:00Z"),
		GroupBy: []string{"model"},
	})
	if err != nil {
		t.Fatalf("Breakdown total: %v", err)
	}
	if len(tot) != 2 {
		t.Fatalf("expected 2 models in total mode, got %d: %+v", len(tot), tot)
	}
}

func TestUsageBreakdown_RejectsUnknownGroupBy(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSQLite(filepath.Join(dir, "u.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	_, _, err = st.Usage().Breakdown(context.Background(), BreakdownOpts{
		GroupBy: []string{"made_up_field"},
	})
	if err == nil {
		t.Fatal("expected error for unknown group_by token")
	}
}

func mustDay(t *testing.T, iso string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Fatalf("parse %s: %v", iso, err)
	}
	return tt
}

// sameSlice treats nil and []string{} as distinct (the allowlist
// semantics depend on the distinction), but slice equality is by value.
func sameSlice(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDesiredPlacements covers CRUD + the restore ordering contract
// (priority DESC, created_at ASC) the lifecycle manager depends on.
func TestDesiredPlacements(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSQLite(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	dp := st.DesiredPlacements()

	rows := []DesiredPlacement{
		{NodeID: "local", ModelID: "chat-70b", Priority: 10, Pinned: true, CreatedAt: time.Unix(1000, 0)},
		{NodeID: "local", ModelID: "embed-small", Priority: 10, CreatedAt: time.Unix(900, 0)},
		{NodeID: "local", ModelID: "vision-12b", Priority: 1, CreatedAt: time.Unix(800, 0)},
		{NodeID: "worker-1", ModelID: "other", Priority: 99, CreatedAt: time.Unix(700, 0)},
	}
	for _, d := range rows {
		if err := dp.Upsert(ctx, d); err != nil {
			t.Fatalf("Upsert(%s): %v", d.ModelID, err)
		}
	}

	got, err := dp.ListByNode(ctx, "local")
	if err != nil {
		t.Fatalf("ListByNode: %v", err)
	}
	wantOrder := []string{"embed-small", "chat-70b", "vision-12b"} // prio 10 (older first), prio 10, prio 1
	if len(got) != len(wantOrder) {
		t.Fatalf("ListByNode: got %d rows, want %d", len(got), len(wantOrder))
	}
	for i, w := range wantOrder {
		if got[i].ModelID != w {
			t.Errorf("order[%d] = %s, want %s", i, got[i].ModelID, w)
		}
	}
	if !got[1].Pinned {
		t.Errorf("chat-70b should round-trip pinned=true")
	}

	// Upsert updates in place (no duplicate row, new priority observed).
	if err := dp.Upsert(ctx, DesiredPlacement{NodeID: "local", ModelID: "vision-12b", Priority: 50}); err != nil {
		t.Fatalf("re-Upsert: %v", err)
	}
	one, err := dp.Get(ctx, "local", "vision-12b")
	if err != nil || one == nil {
		t.Fatalf("Get after re-upsert: %v, %v", one, err)
	}
	if one.Priority != 50 {
		t.Errorf("priority after upsert = %d, want 50", one.Priority)
	}

	if err := dp.Delete(ctx, "local", "vision-12b"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if g, _ := dp.Get(ctx, "local", "vision-12b"); g != nil {
		t.Errorf("Get after delete: want nil, got %+v", g)
	}
	// Missing row is (nil, nil), not an error.
	if g, err := dp.Get(ctx, "local", "never-existed"); err != nil || g != nil {
		t.Errorf("Get(missing) = %+v, %v; want nil, nil", g, err)
	}
}

// TestPlacementSetStatus verifies the draining flip hides a placement
// from GetByModel (the router's view) and that flipping back restores it.
func TestPlacementSetStatus(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSQLite(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	p := st.Placements()

	if err := p.Upsert(ctx, Placement{NodeID: "local", ModelID: "m1", Status: "ready", LastSeen: time.Now()}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if got, _ := p.GetByModel(ctx, "m1"); len(got) != 1 {
		t.Fatalf("GetByModel before drain: got %d, want 1", len(got))
	}
	if err := p.SetStatus(ctx, "local", "m1", "draining"); err != nil {
		t.Fatalf("SetStatus(draining): %v", err)
	}
	if got, _ := p.GetByModel(ctx, "m1"); len(got) != 0 {
		t.Errorf("GetByModel while draining: got %d, want 0 (router must skip)", len(got))
	}
	if err := p.SetStatus(ctx, "local", "m1", "ready"); err != nil {
		t.Fatalf("SetStatus(ready): %v", err)
	}
	if got, _ := p.GetByModel(ctx, "m1"); len(got) != 1 {
		t.Errorf("GetByModel after restore: got %d, want 1", len(got))
	}
	// Unknown placement errors rather than silently no-oping.
	if err := p.SetStatus(ctx, "local", "ghost", "draining"); err == nil {
		t.Errorf("SetStatus(missing placement): want error, got nil")
	}
}

// TestLastUsedByModel verifies the per-model MAX(ts) rollup that drives
// LRU eviction ordering.
func TestLastUsedByModel(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenSQLite(filepath.Join(dir, "x.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	recs := []Usage{
		{TS: time.Unix(100, 0), APIKeyID: "k", UserID: "u", Model: "a", Protocol: "openai", Outcome: "ok"},
		{TS: time.Unix(300, 0), APIKeyID: "k", UserID: "u", Model: "a", Protocol: "openai", Outcome: "ok"},
		{TS: time.Unix(200, 0), APIKeyID: "k", UserID: "u", Model: "b", Protocol: "openai", Outcome: "ok"},
	}
	for _, u := range recs {
		if err := st.Usage().Record(ctx, u); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	got, err := st.Usage().LastUsedByModel(ctx)
	if err != nil {
		t.Fatalf("LastUsedByModel: %v", err)
	}
	if got["a"].Unix() != 300 {
		t.Errorf("a last used = %d, want 300 (MAX of 100,300)", got["a"].Unix())
	}
	if got["b"].Unix() != 200 {
		t.Errorf("b last used = %d, want 200", got["b"].Unix())
	}
	if _, ok := got["never-used"]; ok {
		t.Errorf("models with no usage must be absent from the map")
	}
}
