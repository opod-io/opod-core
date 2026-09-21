package controlplane

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// T11.3 — a gang's pressure comes from its COORDINATOR.
//
// A gang's parts are rpc-servers: they hold weights and multiply matrices,
// they run no engine and have no queue, no KV cache and no notion of a
// request, so they send no engine sample and never will. The process that has
// all three is the coordinator, and it is not a registered worker's engine —
// so nothing that walks the nodes sees it.
//
// Measured on the design-partner cell (2026-09-20): a two-part gang answering
// requests reported kv_used_pct 0, queue_depth 0 and tokens_per_s 0, so two of
// the five triggers the control plane renders for a gang could not fire and
// the endpoint could only ever scale on in-flight, RPM and unavailability.
func TestLoadzReadsGangPressureFromTheCoordinator(t *testing.T) {
	ctx := context.Background()
	var scrapes int64
	coord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&scrapes, 1)
		fmt.Fprint(w, "llamacpp:kv_cache_usage_ratio 0.62\n"+
			"llamacpp:requests_deferred 4\n"+
			"llamacpp:tokens_predicted_total 100\n")
	}))
	defer coord.Close()

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
	// The head on n1 runs the coordinator; n2 holds an rpc part. Neither node
	// reports an engine sample — which is the real situation, not a gap in
	// the fixture.
	gang := []store.Shard{
		{ID: "c", ModelID: "sharded", GangID: "g0", Role: "coordinator", NodeID: "n1", Address: strings.TrimPrefix(coord.URL, "http://"), Status: "ready", CreatedAt: now, LastSeen: now},
		{ID: "p", ModelID: "sharded", GangID: "g0", Role: "rpc", NodeID: "n2", Address: "n2:50052", Status: "ready", CreatedAt: now, LastSeen: now},
	}
	for _, sh := range gang {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}

	var out adminapi.Load
	out.PlanModel = "sharded"
	srv.aggregateWorkerLoad(ctx, &out, now)

	if out.Workers != 2 {
		t.Fatalf("both parts are capacity: workers=%d, want 2", out.Workers)
	}
	if out.Reporting != 1 {
		t.Errorf("one gang, one sample: reporting=%d, want 1", out.Reporting)
	}
	if out.KVUsedPct < 61.9 || out.KVUsedPct > 62.1 {
		t.Errorf("kv_used_pct = %v, want the coordinator's 62 — it holds the KV cache", out.KVUsedPct)
	}
	if out.QueueDepth != 4 {
		t.Errorf("queue_depth = %d, want the coordinator's 4 — the parts have no queue", out.QueueDepth)
	}
	if out.TokensPerSec < 0 {
		t.Errorf("tokens_per_s must not be negative: %v", out.TokensPerSec)
	}

	// The DRIVER is kept between scrapes, not rebuilt. tokens_per_s is a rate
	// the driver holds between samples, so a fresh one per scrape reports 0 for
	// ever — which is what the design-partner cell showed (2026-09-20): a gang
	// under 16 concurrent requests, kv_used_pct rising, tokens_per_s flat 0.
	eng1, _ := srv.gangEngine(gang[0])
	eng2, _ := srv.gangEngine(gang[0])
	if eng1 != eng2 {
		t.Error("the coordinator's driver must be the same instance across scrapes, or its rate has no history")
	}
	moved := gang[0]
	moved.Address = "10.0.0.9:9100"
	if eng3, _ := srv.gangEngine(moved); eng3 == eng1 {
		t.Error("a gang that re-formed at another address needs a new driver, not a client pointed at a pod that is gone")
	}

	// /loadz is unauthenticated and probe-grade: polling it faster must not
	// become the coordinator's scrape rate.
	for i := 0; i < 5; i++ {
		var again adminapi.Load
		again.PlanModel = "sharded"
		srv.aggregateWorkerLoad(ctx, &again, now)
		if again.QueueDepth != 4 {
			t.Fatalf("a cached sample is still the sample: queue_depth=%d", again.QueueDepth)
		}
	}
	if n := atomic.LoadInt64(&scrapes); n != 1 {
		t.Errorf("six reads of /loadz inside the TTL scraped the coordinator %d times, want 1", n)
	}

	// A gang that has lost a part is not capacity and is not scraped for
	// pressure either — its numbers would describe something that cannot
	// take a request.
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "n2", Hostname: "n2", State: "ready", LastHeartbeat: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var broken adminapi.Load
	broken.PlanModel = "sharded"
	srv.aggregateWorkerLoad(ctx, &broken, now)
	if broken.Workers != 0 || broken.Reporting != 0 || broken.KVUsedPct != 0 {
		t.Errorf("a gang missing a part reports nothing: %+v", broken)
	}
}
