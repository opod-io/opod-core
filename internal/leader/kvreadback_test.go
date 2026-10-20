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

	"github.com/opod-io/opod-sdk/adminapi"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// PLAN T23.2: what each engine says it built rides its worker's heartbeat. The
// leader keeps each worker's figures (GET /admin/v1/nodes `load`) and sums
// them on /loadz — a sum over the workers that state the figure, absent while
// none does, never a zero for "the engine did not say".
func TestTheEnginesKVPoolsAreKeptPerWorkerAndSummed(t *testing.T) {
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
	for _, id := range []string{"w1", "w2", "w3"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	heartbeat := func(id, load string) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat",
			strings.NewReader(`{"id":"`+id+`","loaded_models":["m"],"load":`+load+`}`)))
		if rec.Code != http.StatusOK {
			t.Fatalf("heartbeat %s: %d %s", id, rec.Code, rec.Body.String())
		}
	}
	loadz := func() (adminapi.Load, map[string]any) {
		rec := httptest.NewRecorder()
		srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
		var out adminapi.Load
		var raw map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		_ = json.Unmarshal(rec.Body.Bytes(), &raw)
		return out, raw
	}

	// Nobody states a pool yet: the keys are absent, not zero.
	for _, id := range []string{"w1", "w2", "w3"} {
		heartbeat(id, `{"kv_used_pct":10}`)
	}
	ld, raw := loadz()
	for _, k := range []string{"kv_capacity_tokens", "kv_used_tokens", "preemptions_total", "evictions_total"} {
		if _, ok := raw[k]; ok {
			t.Errorf("%s is on /loadz with no engine stating it: %v", k, raw[k])
		}
	}
	if ld.Reporting != 3 {
		t.Fatalf("reporting %d", ld.Reporting)
	}

	heartbeat("w1", `{"kv_used_pct":50,"kv_capacity_tokens":49152,"preemptions_total":2}`)                         // vLLM
	heartbeat("w2", `{"kv_used_pct":25,"kv_capacity_tokens":120000,"kv_used_tokens":30000,"preemptions_total":1}`) // SGLang
	heartbeat("w3", `{"kv_used_pct":10}`)                                                                          // says none of it
	ld, raw = loadz()
	if ld.KVCapacityTokens == nil || *ld.KVCapacityTokens != 49152+120000 {
		t.Fatalf("pool sum: %v", ld.KVCapacityTokens)
	}
	if ld.KVUsedTokens == nil || *ld.KVUsedTokens != 30000 {
		t.Fatalf("used sum is over the engines that count it: %v", ld.KVUsedTokens)
	}
	if ld.PreemptionsTotal == nil || *ld.PreemptionsTotal != 3 {
		t.Fatalf("preemptions sum: %v", ld.PreemptionsTotal)
	}
	if _, ok := raw["evictions_total"]; ok {
		t.Fatalf("no engine counts evictions here: %v", raw["evictions_total"])
	}

	// Each worker's own figures, on the node list.
	rec := httptest.NewRecorder()
	srv.listNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/nodes", nil))
	var nodes []struct {
		ID   string              `json:"id"`
		Load *engines.EngineLoad `json:"load"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &nodes); err != nil {
		t.Fatal(err)
	}
	got := map[string]*engines.EngineLoad{}
	for _, n := range nodes {
		got[n.ID] = n.Load
	}
	if l := got["w2"]; l == nil || l.KVCapacityTokens == nil || *l.KVCapacityTokens != 120000 || l.KVUsedTokens == nil || *l.KVUsedTokens != 30000 {
		t.Fatalf("w2's own figures: %+v", l)
	}
	if l := got["w3"]; l == nil || l.KVCapacityTokens != nil || l.KVUsedPct != 10 {
		t.Fatalf("w3 states no pool, and its sample is still there: %+v", l)
	}

	// A stale sample leaves both views.
	srv.nodeLoad.Store("w1", nodeLoadSample{at: time.Now().Add(-time.Minute)})
	if ld, _ := loadz(); ld.KVCapacityTokens == nil || *ld.KVCapacityTokens != 120000 {
		t.Fatalf("a stale sample's pool must leave the sum: %v", ld.KVCapacityTokens)
	}
}
