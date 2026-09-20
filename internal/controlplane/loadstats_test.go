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
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// A probe is served like any other request and counted like none of them.
//
// The loop this closes was observed on the design-partner cell (2026-09-20):
// the autoscaler parked a gang, the control plane re-proved the endpoint
// because its pod set had changed, the leader counted that proof as demand,
// and KEDA woke the gang 40 seconds after parking it — a model reload on two
// GPUs for nobody.
func TestProbeIsNotDemand(t *testing.T) {
	srv, ts, _ := drainFixture(t)
	read := func() adminapi.Load {
		rec := httptest.NewRecorder()
		srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
		var out adminapi.Load
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := read()

	// A probe that SERVES still moves nothing an autoscaler reads.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(drainChatBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(ProbeHeader, "1")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a probe must still be served: %d", resp.StatusCode)
	}
	after := read()
	if after.RPM1m != before.RPM1m {
		t.Errorf("probe counted in rpm_1m: %d → %d", before.RPM1m, after.RPM1m)
	}
	if after.LastRequestUnix != before.LastRequestUnix {
		t.Error("a probe must not reset the idle clock, or nothing with floor 0 ever idles")
	}

	// A probe that FAILS must not trip unavailable_1m either — that is the
	// trigger that wakes a parked endpoint. Both workers drained is the
	// leader's own "nothing can take this" answer, the 503 a parked endpoint
	// gives.
	for _, id := range []string{"w1", "w2"} {
		if err := srv.DrainNode(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	req2, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(drainChatBody))
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set(ProbeHeader, "1")
	resp2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode < 500 {
		t.Fatalf("fixture changed: wanted a 5xx to count, got %d", resp2.StatusCode)
	}
	if got := read(); got.Unavailable1m != before.Unavailable1m {
		t.Errorf("failed probe counted in unavailable_1m: %d → %d — this is what wakes a parked gang", before.Unavailable1m, got.Unavailable1m)
	}

	// The same request WITHOUT the header is demand, as it always was.
	for _, id := range []string{"w1", "w2"} {
		if err := srv.UndrainNode(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("ordinary chat: %d %s", resp.StatusCode, out)
	}
	if got := read(); got.RPM1m != before.RPM1m+1 {
		t.Errorf("ordinary traffic must still count: rpm_1m %d → %d", before.RPM1m, got.RPM1m)
	}
}

// A sharded endpoint's capacity is its gang's parts. They hold no placement
// row — the router reaches them through the gang — so counting workers by
// placements counted none of them: on the design-partner cell (2026-09-20) a
// two-part gang answered four completions while /loadz reported workers 0,
// reporting 0, kv_used_pct 0, queue_depth 0 and tokens_per_s 0. Three of the
// five triggers the control plane renders for a gang read that document.
func TestLoadzCountsGangParts(t *testing.T) {
	ctx := context.Background()
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
	for _, sh := range []store.Shard{
		{ID: "c", ModelID: "sharded", GangID: "g0", Role: "coordinator", NodeID: "n1", Address: "n1:9001", Status: "ready", CreatedAt: now, LastSeen: now},
		{ID: "p", ModelID: "sharded", GangID: "g0", Role: "rpc", NodeID: "n2", Address: "n2:50052", Status: "ready", CreatedAt: now, LastSeen: now},
	} {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	// Each part reports its engine's pressure, as any worker does.
	srv.nodeLoad.Store("n1", nodeLoadSample{EngineLoad: engines.EngineLoad{KVUsedPct: 40, QueueDepth: 2, TokensPerSec: 30}, at: now})
	srv.nodeLoad.Store("n2", nodeLoadSample{EngineLoad: engines.EngineLoad{KVUsedPct: 70, QueueDepth: 3, TokensPerSec: 20}, at: now})

	var out adminapi.Load
	out.PlanModel = "sharded"
	srv.aggregateWorkerLoad(ctx, &out, now)

	if out.Workers != 2 || out.Reporting != 2 {
		t.Fatalf("a serving gang is capacity: workers=%d reporting=%d, want 2/2", out.Workers, out.Reporting)
	}
	if out.KVUsedPct != 70 {
		t.Errorf("kv_used_pct = %v, want the fullest part's 70 — one full cache is pressure", out.KVUsedPct)
	}
	if out.QueueDepth != 5 {
		t.Errorf("queue_depth = %d, want the gang's 5", out.QueueDepth)
	}
	if out.TokensPerSec != 50 {
		t.Errorf("tokens_per_s = %v, want the gang's 50", out.TokensPerSec)
	}

	// A gang that has LOST a part is not capacity: it cannot take a request,
	// and counting its survivors would hold a scaler back from replacing it.
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "n2", Hostname: "n2", State: "ready", LastHeartbeat: now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	var broken adminapi.Load
	broken.PlanModel = "sharded"
	srv.aggregateWorkerLoad(ctx, &broken, now)
	if broken.Workers != 0 {
		t.Errorf("a gang missing a part is not capacity: workers=%d, want 0", broken.Workers)
	}
}
