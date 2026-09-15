package controlplane

// R10.1: a worker whose PROCESS changed (boot id) under the same node id is
// a new incarnation — its previous placements and every shard group with a
// part on it are dropped at once, without a process-list answer.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

func TestIncarnation(t *testing.T) {
	cases := []struct {
		recorded, presented string
		want                incarnationKind
	}{
		{"a", "a", incarnationSame}, {"", "", incarnationUnknown},
		{"a", "b", incarnationNew},
		{"", "b", incarnationUnknown}, {"a", "", incarnationUnknown},
	}
	for _, c := range cases {
		if got := incarnation(c.recorded, c.presented); got != c.want {
			t.Errorf("incarnation(%q, %q) = %v, want %v", c.recorded, c.presented, got, c.want)
		}
	}
}

func TestNewBootIDDropsThePreviousIncarnation(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	// The worker's own /v1/process/stop: the shard teardown reaches it.
	stops := 0
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/process/stop") {
			stops++
		}
		w.WriteHeader(200)
	}))
	defer worker.Close()
	orch := scheduler.New(st, nil, log, "")
	orch.HTTP = &http.Client{Timeout: 2 * time.Second}
	cfg := config.Default()
	cfg.Listen = ":0"
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, orch)
	admin := Caller{Admin: true}
	addr := strings.TrimPrefix(worker.URL, "http://")

	if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: "n1", Address: addr, BootID: "boot-a"}, admin, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "n1", BootID: "boot-a", LoadedModels: []string{"m"}}, admin); err != nil {
		t.Fatal(err)
	}
	if err := st.Shards().Create(ctx, store.Shard{ID: "s1", ModelID: "m", Role: "rpc", NodeID: "n1", Address: addr, ProcessID: "p1", Status: "ready", CreatedAt: time.Now(), LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if pl, _ := st.Placements().GetByNode(ctx, "n1"); len(pl) != 1 {
		t.Fatalf("placement recorded: %v", pl)
	}

	// Same process registering again (a leader restart): nothing dropped.
	if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: "n1", Address: addr, BootID: "boot-a"}, admin, "tok"); err != nil {
		t.Fatal(err)
	}
	if pl, _ := st.Placements().GetByNode(ctx, "n1"); len(pl) != 1 {
		t.Fatalf("same boot id must keep the placements: %v", pl)
	}
	if sh, _ := st.Shards().GetByModel(ctx, "m"); len(sh) != 1 {
		t.Fatalf("same boot id must keep the shard rows: %v", sh)
	}

	// A new process behind the same id, announced by a heartbeat (a
	// container restarted in place — same pod, same address).
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "n1", BootID: "boot-b"}, admin); err != nil {
		t.Fatal(err)
	}
	if pl, _ := st.Placements().GetByNode(ctx, "n1"); len(pl) != 0 {
		t.Fatalf("new boot id: placements of the previous process must be gone, got %v", pl)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		sh, _ := st.Shards().GetByModel(ctx, "m")
		if len(sh) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("new boot id: the shard group with a part on the node must be removed, still %v", sh)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if stops == 0 {
		t.Fatal("the teardown must ask the worker to stop the recorded process")
	}
	n, _ := st.Nodes().Get(ctx, "n1")
	if n.BootID != "boot-b" {
		t.Fatalf("row carries the new boot id: %q", n.BootID)
	}
	events, _ := st.EventLog().After(ctx, 0, 100)
	seen := map[string]bool{}
	for _, e := range events {
		seen[e.Type] = true
	}
	if !seen["node.reincarnated"] || !seen["shard.stale"] {
		t.Fatalf("events node.reincarnated + shard.stale expected, got %v", seen)
	}
}
