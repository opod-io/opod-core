package scheduler

// A worker that registers again is a new incarnation: the shard rows the
// leader recorded on it are checked against the worker's own process list,
// and a group whose part is gone is removed (found on the laptop cluster,
// 2026-09-14: a LeaderWorkerSet group recreate left "ready" rows pointing at
// a coordinator address that no longer answered).

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/store"
)

func fakeWorker(t *testing.T, running ...string) (*httptest.Server, *int) {
	t.Helper()
	stops := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/process/list":
			var procs []agent.ProcessInfo
			for _, id := range running {
				procs = append(procs, agent.ProcessInfo{ID: id, Status: "running"})
			}
			if procs == nil {
				procs = []agent.ProcessInfo{}
			}
			_ = json.NewEncoder(w).Encode(procs)
		case "/v1/process/stop":
			stops++
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &stops
}

func seedGang(t *testing.T, st store.Store, node string) {
	t.Helper()
	ctx := context.Background()
	for _, s := range []store.Shard{
		{ID: "s-m-coord", ModelID: "m", Role: "coordinator", NodeID: node, Address: "10.0.0.1:9001", ProcessID: "s-m-coord", Status: "ready"},
		{ID: "s-m-rpc-0", ModelID: "m", Role: "rpc", NodeID: node, Address: "10.0.0.1:50052", ProcessID: "s-m-rpc-0", Status: "ready"},
		{ID: "s-m-rpc-1", ModelID: "m", Role: "rpc", NodeID: "other", Address: "10.0.0.2:50052", ProcessID: "s-m-rpc-1", Status: "ready"},
	} {
		if err := st.Shards().Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReconcileNodeRemovesAGroupWhosePartDied(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w, stops := fakeWorker(t, "something-else") // the recorded processes are not running
	node := store.Node{ID: "n1", Address: strings.TrimPrefix(w.URL, "http://"), WorkerToken: "tok"}
	if err := st.Nodes().Upsert(ctx, node); err != nil {
		t.Fatal(err)
	}
	seedGang(t, st, "n1")
	o := &Orchestrator{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HTTP: http.DefaultClient}
	removed, err := o.ReconcileNode(ctx, node)
	if err != nil || len(removed) != 1 || removed[0] != "m" {
		t.Fatalf("removed %v err %v", removed, err)
	}
	rows, _ := st.Shards().GetByModel(ctx, "m")
	if len(rows) != 0 {
		t.Fatalf("stale rows must be gone: %+v", rows)
	}
	if *stops == 0 {
		t.Fatal("the surviving parts are asked to stop (best effort)")
	}
}

func TestReconcileNodeKeepsALiveGroup(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	w, _ := fakeWorker(t, "s-m-coord", "s-m-rpc-0")
	node := store.Node{ID: "n1", Address: strings.TrimPrefix(w.URL, "http://"), WorkerToken: "tok"}
	if err := st.Nodes().Upsert(ctx, node); err != nil {
		t.Fatal(err)
	}
	seedGang(t, st, "n1")
	o := &Orchestrator{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HTTP: http.DefaultClient}
	removed, err := o.ReconcileNode(ctx, node)
	if err != nil || len(removed) != 0 {
		t.Fatalf("a live group stays: removed %v err %v", removed, err)
	}
	rows, _ := st.Shards().GetByModel(ctx, "m")
	if len(rows) != 3 {
		t.Fatalf("rows kept: %d", len(rows))
	}
	// No rows on the node at all → nothing asked of the worker.
	removed, err = o.ReconcileNode(ctx, store.Node{ID: "n9", Address: "127.0.0.1:1"})
	if err != nil || len(removed) != 0 {
		t.Fatalf("no rows → no call: %v %v", removed, err)
	}
	// A worker that does not answer leaves the rows alone and reports the error.
	w.Close()
	if _, err := o.ReconcileNode(ctx, node); err == nil {
		t.Fatal("an unreachable worker is an error, not a removal")
	}
	if rows, _ := st.Shards().GetByModel(ctx, "m"); len(rows) != 3 {
		t.Fatal("rows untouched when the worker did not answer")
	}
}
