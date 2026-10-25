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

// PLAN T26.4: the engines' host KV tiers are summed on /loadz the same way —
// counters over the workers that state them, the fill the fullest tier's, and
// every key absent while no engine keeps a tier.
func TestTheEnginesKVTiersAreSummed(t *testing.T) {
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
	for _, id := range []string{"w1", "w2"} {
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
	keys := []string{"tier_hit_tokens_total", "tier_query_tokens_total", "tier_load_bytes_total", "tier_store_bytes_total", "tier_used_pct", "tier_alloc_failures_total"}

	heartbeat("w1", `{"kv_used_pct":10}`)
	heartbeat("w2", `{"kv_used_pct":10}`)
	if _, raw := loadz(); true {
		for _, k := range keys {
			if _, ok := raw[k]; ok {
				t.Errorf("%s is on /loadz with no tier: %v", k, raw[k])
			}
		}
	}

	heartbeat("w1", `{"kv_used_pct":10,"tier_hit_tokens_total":12416,"tier_query_tokens_total":25000,"tier_load_bytes_total":356000000,"tier_store_bytes_total":357000000,"tier_used_pct":20,"tier_alloc_failures_total":1}`)
	heartbeat("w2", `{"kv_used_pct":10,"tier_hit_tokens_total":584,"tier_query_tokens_total":1000,"tier_used_pct":35}`)
	ld, _ := loadz()
	for name, c := range map[string]struct {
		got  *int64
		want int64
	}{
		"hits":    {ld.TierHitTokensTotal, 13000},
		"queries": {ld.TierQueryTokensTotal, 26000},
		"loaded":  {ld.TierLoadBytesTotal, 356000000},
		"stored":  {ld.TierStoreBytesTotal, 357000000},
		"refused": {ld.TierAllocFailuresTotal, 1},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s: %v, want %d", name, c.got, c.want)
		}
	}
	if ld.TierUsedPct == nil || *ld.TierUsedPct != 35 {
		t.Errorf("fill is the fullest tier's: %v", ld.TierUsedPct)
	}
}
