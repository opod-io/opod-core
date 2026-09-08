package controlplane

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

	"github.com/opod-io/opod-sdk/adminapi"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// Build item 14: a worker's engine load rides its heartbeat and /loadz
// aggregates the live workers — max KV use, summed queue and tokens/s;
// a dead worker's sample and a stale sample drop out.
func TestLoadzAggregatesWorkerLoad(t *testing.T) {
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
	for _, id := range []string{"w1", "w2"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat := func(id, load string) {
		body := `{"id":"` + id + `","loaded_models":["m"]` + load + `}`
		rec := httptest.NewRecorder()
		srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	loadz := func() adminapi.Load {
		rec := httptest.NewRecorder()
		srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
		var out adminapi.Load
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	heartbeat("w1", `,"load":{"kv_used_pct":40,"queue_depth":1,"tokens_per_s":12.5,"prefix_hit_pct":50}`)
	heartbeat("w2", `,"load":{"kv_used_pct":90,"queue_depth":2,"tokens_per_s":7.5,"prefix_hit_pct":10}`)
	ld := loadz()
	if ld.Workers != 2 || ld.Reporting != 2 || ld.KVUsedPct != 90 || ld.QueueDepth != 3 || ld.TokensPerSec != 20 || ld.PrefixHitPct != 30 {
		t.Fatalf("aggregate %+v", ld)
	}
	// a worker without a sample counts as alive but not reporting
	heartbeat("w1", ``)
	if ld := loadz(); ld.Workers != 2 || ld.Reporting != 2 {
		t.Fatalf("a heartbeat without load keeps the last sample: %+v", ld)
	}
	// w2 stops heartbeating → its pressure leaves /loadz
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w2", Hostname: "w2", State: "ready", LastHeartbeat: time.Now().Add(-2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if ld := loadz(); ld.Workers != 1 || ld.Reporting != 1 || ld.KVUsedPct != 40 || ld.QueueDepth != 1 {
		t.Fatalf("dead worker must drop out: %+v", ld)
	}
	// a stale sample (engine stuck) stops reporting
	srv.nodeLoad.Store("w1", nodeLoadSample{at: time.Now().Add(-time.Minute)})
	if ld := loadz(); ld.Workers != 1 || ld.Reporting != 0 || ld.KVUsedPct != 0 {
		t.Fatalf("stale sample must not report: %+v", ld)
	}
}
