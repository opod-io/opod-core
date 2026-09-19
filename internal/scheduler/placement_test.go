package scheduler

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// One rule for "which rows are workers" (WorkerFor), asked by the leader's own
// pick exactly as by the CLI's picker: the leader's "local" row is never one,
// however much RAM it reports, and neither is a draining or a silent node.
func TestPickWorkersAsksTheWorkerRule(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, now := context.Background(), time.Now()
	for _, n := range []store.Node{
		{ID: "local", RAMGB: 2048, Address: "127.0.0.1:8080", State: "ready", LastHeartbeat: now},
		{ID: "small", RAMGB: 32, Address: "192.0.2.1:8081", State: "ready", LastHeartbeat: now},
		{ID: "big", RAMGB: 256, Address: "192.0.2.2:8081", State: "ready", LastHeartbeat: now},
		{ID: "drained", RAMGB: 512, Address: "192.0.2.3:8081", State: "draining", LastHeartbeat: now},
		{ID: "silent", RAMGB: 512, Address: "192.0.2.4:8081", State: "ready", LastHeartbeat: now.Add(-time.Hour)},
		{ID: "nowhere", RAMGB: 512, State: "ready", LastHeartbeat: now},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	got, err := o.pickWorkers(ctx, 2)
	if err != nil || len(got) != 2 || got[0].ID != "big" || got[1].ID != "small" {
		t.Fatalf("pickWorkers(2) = %+v, %v; want big, small", got, err)
	}
	if _, err := o.pickWorkers(ctx, 3); err == nil || !strings.Contains(err.Error(), "need 3 ready workers, have 2") {
		t.Fatalf("pickWorkers(3): %v", err)
	}
	for _, id := range []string{"local", "drained", "silent", "nowhere"} {
		if _, err := o.pickWorkersByID(ctx, []string{"big", id}); err == nil || !strings.Contains(err.Error(), id) {
			t.Errorf("naming %q: %v, want a refusal that names it", id, err)
		}
	}
	// The CLI's picker sees the same two.
	facts, err := WorkerMemoryFacts(ctx, st, nil, "", 20, 0, now)
	if err != nil || len(facts) != 2 {
		t.Fatalf("WorkerMemoryFacts = %+v, %v; want the same two workers", facts, err)
	}
}
