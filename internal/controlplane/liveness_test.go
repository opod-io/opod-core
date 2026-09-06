package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// deadEngine is a leader with no local engine (router-only / shard mode).
type deadEngine struct{ *stubLeaderEngine }

func (*deadEngine) Health(context.Context) error { return errors.New("no local engine") }

func TestLiveNodeState(t *testing.T) {
	cases := []struct {
		state string
		alive bool
		want  string
	}{
		{"ready", true, "ready"},
		{"ready", false, NodeStateLost},
		{"joining", false, NodeStateLost},
		{"", false, NodeStateLost},
		{"draining", false, "draining"},
		{"draining", true, "draining"},
	}
	for _, c := range cases {
		if got := liveNodeState(store.Node{ID: "n", State: c.state}, c.alive); got != c.want {
			t.Errorf("liveNodeState(%q, alive=%v) = %q, want %q", c.state, c.alive, got, c.want)
		}
	}
	now := time.Now()
	if !nodeAlive(store.Node{ID: "local"}, time.Second, now) {
		t.Error("local must always be alive")
	}
	if nodeAlive(store.Node{ID: "w", LastHeartbeat: now.Add(-31 * time.Second)}, 30*time.Second, now) {
		t.Error("31 s old heartbeat must be dead at 30 s max age")
	}
}

func TestServableShardModels(t *testing.T) {
	shards := []store.Shard{
		{ModelID: "m", Role: "coordinator", NodeID: "a", Status: "ready"},
		{ModelID: "m", Role: "rpc", NodeID: "a", Status: "ready"},
		{ModelID: "m", Role: "rpc", NodeID: "b", Status: "ready"},
		{ModelID: "starting", Role: "coordinator", NodeID: "a", Status: "starting"},
		{ModelID: "leader-hosted", Role: "coordinator", NodeID: "local", Status: "ready"},
	}
	got := servableShardModels(shards, map[string]bool{"local": true, "a": true, "b": true})
	if !got["m"] || got["starting"] || !got["leader-hosted"] {
		t.Errorf("all alive: %v", got)
	}
	got = servableShardModels(shards, map[string]bool{"local": true, "a": true, "b": false})
	if got["m"] {
		t.Errorf("one lost rpc part must take the gang out: %v", got)
	}
	if liveShardStatus(shards[2], map[string]bool{"b": false}) != ShardStatusLost {
		t.Error("part on a lost node must read lost")
	}
}

// TestReadyzFollowsHeartbeats: the leader's readiness and listings track
// heartbeat age — the regression that left a PP2 gang "ready" for 1h45m
// after both worker pods were gone.
func TestReadyzFollowsHeartbeats(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, log, nil)

	readyz := func() (int, string) {
		rec := httptest.NewRecorder()
		srv.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		mode, _ := body["mode"].(string)
		return rec.Code, mode
	}
	nodeState := func(id string) string {
		rec := httptest.NewRecorder()
		srv.listNodes(rec, httptest.NewRequest(http.MethodGet, "/nodes", nil))
		var rows []map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &rows)
		for _, r := range rows {
			if r["ID"] == id {
				s, _ := r["state"].(string)
				return s
			}
		}
		return "<missing>"
	}
	shardStatus := func() string {
		rec := httptest.NewRecorder()
		srv.listShards(rec, httptest.NewRequest(http.MethodGet, "/shards", nil))
		var rows []store.Shard
		_ = json.Unmarshal(rec.Body.Bytes(), &rows)
		if len(rows) == 0 {
			return "<none>"
		}
		return rows[0].Status
	}

	if code, _ := readyz(); code != http.StatusServiceUnavailable {
		t.Fatalf("empty leader readyz = %d, want 503", code)
	}

	// Router-only: one worker with a ready placement.
	worker := store.Node{ID: "w1", Hostname: "w1", State: "ready", LastHeartbeat: time.Now()}
	if err := st.Nodes().Upsert(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if err := st.Placements().Upsert(ctx, store.Placement{NodeID: "w1", ModelID: "m", Status: "ready", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if code, mode := readyz(); code != http.StatusOK || mode != "router-only" {
		t.Fatalf("fresh worker readyz = %d/%s, want 200/router-only", code, mode)
	}
	if got := nodeState("w1"); got != "ready" {
		t.Errorf("fresh worker state = %q", got)
	}

	// Heartbeats stop.
	worker.LastHeartbeat = time.Now().Add(-2 * time.Minute)
	if err := st.Nodes().Upsert(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if code, _ := readyz(); code != http.StatusServiceUnavailable {
		t.Fatalf("stale worker readyz = %d, want 503", code)
	}
	if got := nodeState("w1"); got != NodeStateLost {
		t.Errorf("stale worker state = %q, want lost", got)
	}
	if srv.hasServingCapacity(ctx) {
		t.Error("hasServingCapacity must be false with only a stale worker")
	}

	// Sharded: coordinator + rpc part on the same stale worker → still 503;
	// worker returns → ready in shard-coordinator mode; part on a second
	// worker that goes stale → 503 again (one lost part takes the gang out).
	if err := st.Placements().Delete(ctx, "w1", "m"); err != nil {
		t.Fatal(err)
	}
	for _, sh := range []store.Shard{
		{ID: "c", ModelID: "g", Role: "coordinator", NodeID: "w1", Address: "w1:9001", Status: "ready", CreatedAt: time.Now(), LastSeen: time.Now()},
		{ID: "p", ModelID: "g", Role: "rpc", NodeID: "w2", Address: "w2:50052", Status: "ready", CreatedAt: time.Now(), LastSeen: time.Now()},
	} {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w2", Hostname: "w2", State: "ready", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if code, _ := readyz(); code != http.StatusServiceUnavailable {
		t.Fatalf("gang with stale coordinator node readyz = %d, want 503", code)
	}
	if got := shardStatus(); got != ShardStatusLost {
		t.Errorf("coordinator on stale node status = %q, want lost", got)
	}
	worker.LastHeartbeat = time.Now()
	if err := st.Nodes().Upsert(ctx, worker); err != nil {
		t.Fatal(err)
	}
	if code, mode := readyz(); code != http.StatusOK || mode != "shard-coordinator" {
		t.Fatalf("live gang readyz = %d/%s, want 200/shard-coordinator", code, mode)
	}
	if got := shardStatus(); got != "ready" {
		t.Errorf("live coordinator status = %q, want ready", got)
	}
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w2", Hostname: "w2", State: "ready", LastHeartbeat: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if code, _ := readyz(); code != http.StatusServiceUnavailable {
		t.Fatalf("gang with one lost rpc part readyz = %d, want 503", code)
	}
}
