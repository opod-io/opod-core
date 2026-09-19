package controlplane

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// TestShardCreateNeverPlacesOnTheLeaderRow: POST /admin/v1/shards/create with
// a count and no nodes used to be able to choose the leader's own "local" row
// — ready, with an address — and then fail at /v1/process/start, a route the
// gateway does not have, after tearing down the gang it was replacing. The
// API path now asks the rule the CLI's picker asks (scheduler.WorkerFor) and
// refuses up front, with the numbers, touching nothing.
func TestShardCreateNeverPlacesOnTheLeaderRow(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Every machine answers on the same fake: any call at all is a process
	// launched (or a gang torn down) for a create that should change nothing.
	var calls atomic.Int64
	machine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer machine.Close()
	addr := strings.TrimPrefix(machine.URL, "http://")

	now := time.Now()
	for _, n := range []store.Node{
		{ID: "local", Hostname: "leader", RAMGB: 1024, Address: addr, State: store.NodeStateReady, LastHeartbeat: now},
		{ID: "w1", Hostname: "w1", RAMGB: 64, Address: addr, State: store.NodeStateReady, LastHeartbeat: now},
		{ID: "w2", Hostname: "w2", RAMGB: 64, Address: addr, State: store.NodeStateDraining, LastHeartbeat: now},
		{ID: "w3", Hostname: "w3", RAMGB: 64, Address: addr, State: store.NodeStateReady, LastHeartbeat: now.Add(-10 * time.Minute)},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	// The gang a successful create would replace. A refusal must leave it.
	if err := st.Shards().Create(ctx, store.Shard{ID: "old", ModelID: "big", Role: "coordinator", NodeID: "w1", Address: addr, ProcessID: "p-old", Status: "ready", CreatedAt: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}

	cat := []models.Entry{{ID: "big", Source: models.SourceSpec{Type: "file", Path: "/nonexistent.gguf"},
		Sharding: models.ShardingSpec{Required: true, DefaultShards: 2}}}
	orch := scheduler.New(st, nil, log, t.TempDir())
	orch.HTTP = &http.Client{Timeout: 2 * time.Second}
	cfg := config.Default()
	cfg.Listen = ":0"
	srv := NewServer(cfg, st, &stubLeaderEngine{}, cat, log, orch)
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	plain, rec, err := auth.Generate("shardplace-admin", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	create := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/v1/shards/create", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+plain)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	// A count the real workers cannot meet: one qualifies, two are asked for.
	code, out := create(`{"model_id":"big","shards":2}`)
	if code != http.StatusConflict {
		t.Fatalf("count without enough real workers: %d %s, want 409", code, out)
	}
	for _, want := range []string{
		"need 2 ready workers, have 1 (w1)",
		"local — the leader's own row, not a worker",
		"w2 — state draining",
		"w3 — last heartbeat",
		"nothing was changed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not say %q: %s", want, out)
		}
	}
	// The catalog's default count takes the same road.
	if code, out := create(`{"model_id":"big"}`); code != http.StatusConflict || !strings.Contains(out, "need 2 ready workers, have 1") {
		t.Errorf("default_shards: %d %s", code, out)
	}
	// Naming the leader's row is refused by the same rule, with its reason.
	code, out = create(`{"model_id":"big","nodes":["w1","local"]}`)
	if code != http.StatusConflict || !strings.Contains(out, "local") || !strings.Contains(out, "the leader's own row, not a worker") {
		t.Errorf("named local: %d %s", code, out)
	}

	if n := calls.Load(); n != 0 {
		t.Errorf("a refused create made %d call(s) to the machines; it must touch nothing", n)
	}
	if sh, _ := st.Shards().GetByModel(ctx, "big"); len(sh) != 1 || sh[0].ID != "old" {
		t.Errorf("a refused create must leave the serving gang alone, rows now: %+v", sh)
	}
}
