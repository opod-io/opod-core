package leader

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/store"
)

// PLAN T32.58 — a gang's coordinator row is ready only once its API answers.
//
// Measured on the design-partner cell (2026-11-07): an SGLang tensor-2 gang's
// rows read ready from launch while every request got 503 for 2 min 20 s,
// because the head binds its API only after the group forms. The create now
// stores the coordinator "starting", and this is the loop that moves it.
func TestAStartingGangCoordinatorTurnsReadyOnlyWhenItsAPIAnswers(t *testing.T) {
	ctx := context.Background()
	var answers atomic.Bool
	head := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" || !answers.Load() {
			http.Error(w, "loading", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"sharded","object":"model"}]}`)
	}))
	defer head.Close()

	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &downEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	now := time.Now()
	for _, id := range []string{"n1", "n2"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: now}); err != nil {
			t.Fatal(err)
		}
	}
	// What createShardedSGLang stores: rank 0 is the coordinator, starting;
	// rank 1 only joins the group and serves nothing, so it is ready at launch.
	for _, sh := range []store.Shard{
		{ID: "coord", ModelID: "sharded", GangID: "g0", Role: "coordinator", NodeID: "n1",
			Address: strings.TrimPrefix(head.URL, "http://"), ProcessID: "p0", Status: "starting",
			ConfigJSON: `{"engine":"sglang"}`, CreatedAt: now, LastSeen: now},
		{ID: "rank-1", ModelID: "sharded", GangID: "g0", Role: "rank", NodeID: "n2",
			Address: "n2", ProcessID: "p1", Status: "ready", CreatedAt: now, LastSeen: now},
	} {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	alive := map[string]bool{"n1": true, "n2": true}
	servable := func() bool {
		rows, err := st.Shards().List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return servableShardModels(rows, alive)["sharded"]
	}
	status := func() string {
		sh, err := st.Shards().Get(ctx, "coord")
		if err != nil || sh == nil {
			t.Fatalf("coordinator row: %v", err)
		}
		return sh.Status
	}

	srv.promoteAnsweringCoordinators(ctx)
	if got := status(); got != "starting" {
		t.Fatalf("a head that refuses /v1/models must stay starting, got %q", got)
	}
	if servable() {
		t.Fatal("a gang whose head does not answer must not read servable")
	}

	answers.Store(true)
	srv.promoteAnsweringCoordinators(ctx)
	if got := status(); got != "ready" {
		t.Fatalf("a head that answers must read ready, got %q", got)
	}
	if !servable() {
		t.Fatal("the gang must read servable once its head answers")
	}
}

// A fact learnt about one incarnation of a part must not land on the next: a
// gang re-formed under the same ids between the loop's read and its write is
// a new process that has not answered yet.
func TestUpdateStatusIfLeavesAReformedRowAlone(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first := time.Now().Add(-time.Minute)
	old := store.Shard{ID: "coord", ModelID: "m", GangID: "g0", Role: "coordinator", NodeID: "n1",
		Address: "10.0.0.1:9100", ProcessID: "p0", Status: "starting", CreatedAt: first, LastSeen: first}
	if err := st.Shards().Create(ctx, old); err != nil {
		t.Fatal(err)
	}
	// Re-formed: same id, new pod address and creation time.
	if err := st.Shards().Delete(ctx, "coord"); err != nil {
		t.Fatal(err)
	}
	reformed := old
	reformed.Address, reformed.CreatedAt = "10.0.0.2:9100", time.Now()
	if err := st.Shards().Create(ctx, reformed); err != nil {
		t.Fatal(err)
	}

	moved, err := st.Shards().UpdateStatusIf(ctx, old, "starting", "ready")
	if err != nil {
		t.Fatal(err)
	}
	if moved {
		t.Fatal("the old incarnation's answer moved the re-formed row")
	}
	if sh, _ := st.Shards().Get(ctx, "coord"); sh == nil || sh.Status != "starting" {
		t.Fatalf("the re-formed row must still read starting, got %+v", sh)
	}

	got, _ := st.Shards().Get(ctx, "coord")
	if moved, err := st.Shards().UpdateStatusIf(ctx, *got, "starting", "ready"); err != nil || !moved {
		t.Fatalf("the row's own incarnation must move: moved=%v err=%v", moved, err)
	}
}

// A gang's parts hold no placement row: the worker that runs an SGLang gang's
// rank 0 finds the head on its engine port and reports the model as its own,
// and a row from that report let the router fall through to the worker, and
// /readyz read 200, while the coordinator row still read starting (the
// design-partner cell, 2026-11-07: one chat got 502 in that window).
func TestAGangPartsHeartbeatWritesNoPlacementForTheGangsModel(t *testing.T) {
	srv, _, _ := drainFixture(t) // w1 and w2 registered
	st := srv.store
	ctx := context.Background()
	admin := Caller{Admin: true}
	now := time.Now()
	if err := st.Shards().Create(ctx, store.Shard{ID: "coord", ModelID: "m", GangID: "g0", Role: "coordinator",
		NodeID: "w1", Address: "w1:30000", ProcessID: "p0", Status: "starting", ConfigJSON: `{"engine":"sglang"}`,
		CreatedAt: now, LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"w1", "w2"} {
		if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: id, LoadedModels: []string{"m"}}, admin); err != nil {
			t.Fatal(err)
		}
	}
	held := func(node string) bool {
		ps, err := st.Placements().GetByNode(ctx, node)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.ModelID == "m" {
				return true
			}
		}
		return false
	}
	if held("w1") {
		t.Error("the node running the gang's rank 0 got a placement row for the gang's model")
	}
	if !held("w2") {
		t.Error("a plain worker that reports the model must still get its row")
	}
}

// A node that runs a gang's part, and not its coordinator, serves nothing of
// its own, so its worker's engine report is not what keeps it in rotation:
// once an SGLang group forms, rank 1 binds a dummy health server on the engine
// port, the worker's model list gets an error, and the "no report" that
// followed took the node — and the gang — out 30 s after it began to serve
// (design-partner cell, 2026-11-07). The coordinator's node and a plain
// worker keep the rule.
func TestAGangRankThatSendsNoReportStaysInRotation(t *testing.T) {
	srv, ts, _ := drainFixture(t) // w1 and w2 registered
	ctx := context.Background()
	maxAge := srv.heartbeatMaxAge()
	now := time.Now()
	for _, sh := range []store.Shard{
		{ID: "coord", ModelID: "m", GangID: "g0", Role: "coordinator", NodeID: "w1", Address: "w1:30000",
			ProcessID: "p0", Status: "ready", ConfigJSON: `{"engine":"sglang"}`, CreatedAt: now, LastSeen: now},
		{ID: "rank-1", ModelID: "m", GangID: "g0", Role: "rank", NodeID: "w2", Address: "w2",
			ProcessID: "p1", Status: "ready", CreatedAt: now, LastSeen: now},
	} {
		if err := srv.store.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	silent := func(id string) bool {
		wireHeartbeat(t, ts, `{"id":"`+id+`","loaded_models":null}`)
		n, _ := srv.store.Nodes().Get(ctx, id)
		if n.EngineSilentSince.IsZero() {
			return false
		}
		n.EngineSilentSince = time.Now().Add(-maxAge - 10*time.Second) // silent past the bound
		if err := srv.store.Nodes().Upsert(ctx, *n); err != nil {
			t.Fatal(err)
		}
		wireHeartbeat(t, ts, `{"id":"`+id+`","loaded_models":null}`)
		n, _ = srv.store.Nodes().Get(ctx, id)
		return !n.TakesNewWork(maxAge, time.Now())
	}
	if silent("w2") {
		t.Error("a gang rank whose worker sends no engine report left rotation")
	}
	if !silent("w1") {
		t.Error("the coordinator's node runs the gang's API on its worker's engine port: its silence must still count")
	}
}
