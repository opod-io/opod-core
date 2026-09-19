package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// nodeTestConfig is a CLI config over a temp data dir with a saved admin key
// and a file-backed store holding worker "w1" and its placement.
func nodeTestConfig(t *testing.T, listen string) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.DataDir, cfg.Listen = dir, listen
	cfg.Storage.DSN = filepath.Join(dir, "state.db")
	if err := os.WriteFile(filepath.Join(dir, "admin.key"), []byte("test-admin-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := store.OpenSQLite(cfg.Storage.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", Address: "192.0.2.7:8081", State: store.NodeStateReady, LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Placements().Upsert(ctx, store.Placement{NodeID: "w1", ModelID: "m", Status: "ready", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func storedNode(t *testing.T, cfg *config.Config, id string) (*store.Node, int) {
	t.Helper()
	st, err := store.OpenSQLite(cfg.Storage.DSN)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n, err := st.Nodes().Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := st.Placements().GetByNode(context.Background(), id)
	return n, len(ps)
}

// fakeLeader records the admin calls it is given and answers with status.
func fakeLeader(t *testing.T, status int, body string) (listen string, calls *[]string) {
	t.Helper()
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	return ":" + port, &seen
}

// closedPort is a listen address nobody answers on.
func closedPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	return ":" + port
}

// A running leader must forget a node itself: besides the rows it holds the
// worker's cached connection, cooldown and sticky pins in memory. So `opod
// node remove` asks it, leaves the store to it, and says so; only when no
// leader answers does the command delete the row — and the node's placements
// with it — from the store.
func TestNodeRemoveGoesThroughTheLeader(t *testing.T) {
	listen, calls := fakeLeader(t, http.StatusOK, `{"status":"removed","id":"w1"}`)
	cfg := nodeTestConfig(t, listen)
	via, err := removeNode(cfg, "w1")
	if err != nil || via != viaLeader {
		t.Fatalf("removeNode = %q, %v; want the leader path", via, err)
	}
	if len(*calls) != 1 || (*calls)[0] != "DELETE /admin/v1/nodes/w1 Bearer test-admin-key" {
		t.Fatalf("the leader saw %v", *calls)
	}
	if n, _ := storedNode(t, cfg, "w1"); n == nil {
		t.Fatal("with a leader answering, the CLI must not delete behind its back — the row is the leader's to remove")
	}

	// A leader that answers with a refusal is final: no fallback.
	listen, _ = fakeLeader(t, http.StatusForbidden, `{"error":{"message":"admin scope required"}}`)
	cfg = nodeTestConfig(t, listen)
	if via, err := removeNode(cfg, "w1"); err == nil || via != viaLeader || !strings.Contains(err.Error(), "admin scope required") {
		t.Fatalf("a refusing leader: %q, %v", via, err)
	}
	if n, _ := storedNode(t, cfg, "w1"); n == nil {
		t.Fatal("a refusal must not fall back to the store")
	}

	// No leader: the store, row and placements both.
	cfg = nodeTestConfig(t, closedPort(t))
	via, err = removeNode(cfg, "w1")
	if err != nil || via != viaStore {
		t.Fatalf("no leader: %q, %v; want the store path", via, err)
	}
	if n, placements := storedNode(t, cfg, "w1"); n != nil || placements != 0 {
		t.Fatalf("after a store removal: node %v, %d placement(s) left", n, placements)
	}
	if _, err := removeNode(cfg, "w1"); err == nil || !strings.Contains(err.Error(), "no such node") {
		t.Fatalf("removing an unknown node: %v", err)
	}
}

func TestNodeDrainSaysWhichPathItTook(t *testing.T) {
	listen, calls := fakeLeader(t, http.StatusOK, `{"status":"draining","id":"w1"}`)
	cfg := nodeTestConfig(t, listen)
	if via, err := setNodeState(cfg, "w1", "drain", store.NodeStateDraining); err != nil || via != viaLeader {
		t.Fatalf("with a leader: %q, %v", via, err)
	}
	if len(*calls) != 1 || !strings.HasPrefix((*calls)[0], "POST /admin/v1/nodes/w1/drain ") {
		t.Fatalf("the leader saw %v", *calls)
	}
	cfg = nodeTestConfig(t, closedPort(t))
	if via, err := setNodeState(cfg, "w1", "drain", store.NodeStateDraining); err != nil || via != viaStore {
		t.Fatalf("no leader: %q, %v", via, err)
	}
	if n, _ := storedNode(t, cfg, "w1"); n == nil || !n.Draining() {
		t.Fatalf("the store path must write the state: %+v", n)
	}
	if _, err := setNodeState(cfg, "nobody", "drain", store.NodeStateDraining); err == nil {
		t.Fatal("draining an unknown node must be an error")
	}
}

// `opod node ls` shows the state the leader acts on — the router's own rule —
// not the stored column, which says `ready` for ever after a worker dies.
func TestNodeLsShowsTheLiveState(t *testing.T) {
	now := time.Now()
	fresh, silent := now.Add(-3*time.Second), now.Add(-10*time.Minute)
	nodes := []store.Node{
		{ID: "local", State: store.NodeStateReady, LastHeartbeat: silent}, // the leader never heartbeats
		{ID: "up", State: store.NodeStateReady, LastHeartbeat: fresh},
		{ID: "gone", State: store.NodeStateReady, LastHeartbeat: silent},
		{ID: "drained", State: store.NodeStateDraining, LastHeartbeat: fresh},
		{ID: "drained-gone", State: store.NodeStateDraining, LastHeartbeat: silent},
	}
	want := map[string]string{"local": "ready", "up": "ready", "gone": store.NodeStateLost, "drained": "draining", "drained-gone": "draining"}
	for _, seconds := range []int{30, 0} { // 0 = the router's check is off; the leader's own 60 s bound applies
		cfg := config.Default()
		cfg.Router.HeartbeatMaxAgeSeconds = seconds
		for _, r := range nodeRows(cfg, nodes, now) {
			if r.State != want[r.ID] {
				t.Errorf("heartbeat_max_age_seconds=%d: %s shows %q, want %q", seconds, r.ID, r.State, want[r.ID])
			}
		}
	}
}
