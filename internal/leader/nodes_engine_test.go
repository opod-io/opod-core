package leader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/nodeapi"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// /loadz counts the workers whose engine is not serving; the node list says
// which ones (feature engine_liveness). A manager that sees the count alone
// cannot tell which pod holds a card with a dead engine, nor which card. The
// row carries the worker's last word while it is fresh, and nothing when it is
// stale or was never said — the same 30 s rule as the count.
func TestTheNodeListSaysWhichWorkersEngineIsDown(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	for _, id := range []string{"w1", "w2", "w3", "w4"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	// w1 says crash-looping in a heartbeat, the way a worker does.
	rec := httptest.NewRecorder()
	srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat",
		strings.NewReader(`{"id":"w1","loaded_models":[],"engine":{"state":"crash-looping","restarts":5,"since":"2026-10-28T05:06:18Z","detail":"signal: killed"}}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	srv.nodeEngine.Store("w2", nodeEngineSample{EngineState: nodeapi.EngineState{State: nodeapi.EngineServing}, at: time.Now()})
	// w3 said crash-looping five minutes ago and nothing since: stale, so not said.
	srv.nodeEngine.Store("w3", nodeEngineSample{EngineState: nodeapi.EngineState{State: nodeapi.EngineCrashLooping}, at: time.Now().Add(-5 * time.Minute)})
	// w4 never said anything.

	rec = httptest.NewRecorder()
	srv.listNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/nodes", nil))
	var rows []struct {
		ID     string               `json:"ID"`
		Engine *nodeapi.EngineState `json:"engine"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	got := map[string]*nodeapi.EngineState{}
	for _, r := range rows {
		got[r.ID] = r.Engine
	}
	if e := got["w1"]; e == nil || e.State != nodeapi.EngineCrashLooping || e.Restarts != 5 || e.Detail != "signal: killed" || e.Since == "" {
		t.Fatalf("w1 must carry its engine statement: %+v", e)
	}
	if e := got["w2"]; e == nil || e.State != nodeapi.EngineServing {
		t.Fatalf("w2 serving: %+v", e)
	}
	if got["w3"] != nil || got["w4"] != nil {
		t.Fatalf("a stale or missing statement is absent, never guessed: w3 %+v w4 %+v", got["w3"], got["w4"])
	}
}
