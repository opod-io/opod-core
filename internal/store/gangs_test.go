package store

import (
	"context"
	"testing"
	"time"
)

func gangPart(model, gang, role, node string) Shard {
	return Shard{
		ID: "s-" + model + "-" + gang + "-" + role, ModelID: model, GangID: gang,
		Role: role, NodeID: node, Address: node + ":9001", ProcessID: "p-" + model + "-" + gang + "-" + role,
		Status: "ready", CreatedAt: time.Now(), LastSeen: time.Now(),
	}
}

// The model id used to BE the gang identity: a model had one gang, and every
// query said "by model". Two gangs of one model must now be addressable
// separately — read, listed and deleted — or the second create silently
// overwrites the first.
func TestTwoGangsOfOneModelAreSeparate(t *testing.T) {
	st, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for _, sh := range []Shard{
		gangPart("m", "g0", "coordinator", "n1"),
		gangPart("m", "g0", "rpc-0", "n2"),
		gangPart("m", "g1", "coordinator", "n3"),
		gangPart("m", "g1", "rpc-0", "n4"),
	} {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatalf("create %s: %v", sh.ID, err)
		}
	}

	all, err := st.Shards().GetByModel(ctx, "m")
	if err != nil || len(all) != 4 {
		t.Fatalf("the model has 4 parts across 2 gangs: %d %v", len(all), err)
	}
	for _, gang := range []string{"g0", "g1"} {
		parts, err := st.Shards().GetByGang(ctx, "m", gang)
		if err != nil || len(parts) != 2 {
			t.Fatalf("gang %s has 2 parts: %d %v", gang, len(parts), err)
		}
		for _, p := range parts {
			if p.Gang() != gang {
				t.Errorf("gang %s query returned a part of %s", gang, p.Gang())
			}
		}
	}
	gangs, err := st.Shards().GangsOf(ctx, "m")
	if err != nil || len(gangs) != 2 {
		t.Fatalf("GangsOf: %v %v", gangs, err)
	}

	// Deleting one gang leaves the other whole. Before gangs, "delete the
	// model's shards" was the only delete there was.
	if err := st.Shards().DeleteByGang(ctx, "m", "g0"); err != nil {
		t.Fatal(err)
	}
	left, err := st.Shards().GetByModel(ctx, "m")
	if err != nil || len(left) != 2 {
		t.Fatalf("g1 must survive g0's delete: %d %v", len(left), err)
	}
	for _, p := range left {
		if p.Gang() != "g1" {
			t.Errorf("g0's delete took a part of %s with it", p.Gang())
		}
	}
}

// A gang id is only unique WITHIN a model, so grouping on it alone merges the
// g0 of every sharded model into one set — and every servability question
// asked of that set answers for the wrong model's parts.
func TestGroupGangsKeepsModelsApart(t *testing.T) {
	groups := GroupGangs([]Shard{
		gangPart("alpha", "g0", "coordinator", "n1"),
		gangPart("beta", "g0", "coordinator", "n2"),
		gangPart("beta", "g0", "rpc-0", "n3"),
	})
	if len(groups) != 2 {
		t.Fatalf("two models' g0 are two gangs, not one: %v", groups)
	}
	if got := len(groups[GangKey{Model: "alpha", Gang: "g0"}]); got != 1 {
		t.Errorf("alpha/g0 has 1 part, got %d", got)
	}
	if got := len(groups[GangKey{Model: "beta", Gang: "g0"}]); got != 2 {
		t.Errorf("beta/g0 has 2 parts, got %d", got)
	}
}

// A part built in memory without a gang belongs to the default one, and is
// stored that way — so nothing downstream has to know two spellings for it.
func TestUnnamedGangIsStoredAsTheDefault(t *testing.T) {
	st, err := OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	sh := gangPart("m", "", "coordinator", "n1")
	sh.GangID = ""
	if err := st.Shards().Create(ctx, sh); err != nil {
		t.Fatal(err)
	}
	got, err := st.Shards().Get(ctx, sh.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v", err)
	}
	if got.GangID != DefaultGangID {
		t.Errorf("stored gang = %q, want %q", got.GangID, DefaultGangID)
	}
	parts, err := st.Shards().GetByGang(ctx, "m", "")
	if err != nil || len(parts) != 1 {
		t.Errorf("an empty gang argument means the default gang: %d %v", len(parts), err)
	}
}
