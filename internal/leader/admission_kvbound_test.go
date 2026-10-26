package leader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/router"
	"github.com/opod-io/opod/internal/store"
)

// beatKV is one heartbeat from worker id holding "m" with slots and, when
// pool > 0, the KV pool its engine states.
func beatKV(t *testing.T, srv *Server, id string, slots int, pool int64) {
	t.Helper()
	body := fmt.Sprintf(`{"id":%q,"loaded_models":["m"],"slots":%d`, id, slots)
	if pool > 0 {
		body += fmt.Sprintf(`,"load":{"kv_capacity_tokens":%d,"sampled_at":%d}`, pool, time.Now().Unix())
	}
	rec := httptest.NewRecorder()
	srv.heartbeatNode(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/nodes/heartbeat", strings.NewReader(body+"}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
	}
}

// T23.11 · admission counts the smaller of a worker's slots and the requests
// its KV pool holds at the endpoint's context — and says which.
func TestAdmissionTakesTheKVBoundWhenSmaller(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name      string
		slots     int
		pool      int64
		context   int
		want      int
		wantBound string
	}{
		{"pool holds fewer than the slots", 8, 4096, 1024, 4, "kv"},
		{"a tie stays slots (llama.cpp partitioned slots)", 4, 4096, 1024, 4, "slots"},
		{"the plan states no context", 8, 4096, 0, 8, "slots"},
		{"the engine states no pool", 8, 0, 1024, 8, "slots"},
		{"a pool smaller than one request still holds one", 8, 512, 1024, 1, "kv"},
	} {
		t.Run(c.name, func(t *testing.T) {
			srv := slottedLeader(t, c.slots, PolicyAdmission{})
			srv.plan.mu.Lock()
			srv.plan.modelID, srv.plan.context = "m", c.context
			srv.plan.mu.Unlock()
			beatKV(t, srv, "w1", c.slots, c.pool)
			srv.capacityChanged()

			cv := srv.capacityFor(ctx, "m")
			if cv.slots != c.want {
				t.Fatalf("pool slots = %d, want %d", cv.slots, c.want)
			}
			if got := srv.admissionBound(ctx, "m"); got != c.wantBound {
				t.Fatalf("admission_bound = %q, want %q", got, c.wantBound)
			}
			// The picker lands requests by the same number; the registry keeps
			// the engine's own count.
			if n, _ := srv.routerSlots("w1"); n != c.want {
				t.Fatalf("router slots = %d, want %d", n, c.want)
			}
			if n, _ := srv.slotsOfKey("w1"); n != c.slots {
				t.Fatalf("the engine's own slots must stay %d on the registry, got %d", c.slots, n)
			}
		})
	}
}

// The gate queues at the KV bound: with 8 slots and a pool of 4 requests,
// the fifth request waits and a completion lets it in.
func TestGateQueuesAtTheKVBound(t *testing.T) {
	srv := slottedLeader(t, 8, PolicyAdmission{})
	srv.plan.mu.Lock()
	srv.plan.modelID, srv.plan.context = "m", 1024
	srv.plan.mu.Unlock()
	beatKV(t, srv, "w1", 8, 4096)
	srv.capacityChanged()

	var rels []func()
	for range 4 {
		rels = append(rels, admitNow(t, srv, classStandard, "f"))
	}
	out := make(chan admitted, 1)
	admitAs(srv, context.Background(), "fifth", classStandard, "f", out)
	select {
	case a := <-out:
		t.Fatalf("the fifth request passed a KV bound of 4: %+v", a)
	case <-time.After(150 * time.Millisecond):
	}
	rels[0]()
	select {
	case a := <-out:
		if a.why != "" {
			t.Fatalf("the waiter was shed: %q", a.why)
		}
		a.release()
	case <-time.After(2 * time.Second):
		t.Fatal("a completion did not let the waiter in")
	}
	for _, r := range rels[1:] {
		r()
	}
}

// A gang counts its coordinator's slots lowered by the coordinator's pool, and
// the router reads the gang key's bounded number.
func TestGangSlotsTakeTheCoordinatorsKVBound(t *testing.T) {
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
	coord := store.Shard{ID: "c", ModelID: "big", GangID: "g0", Role: "coordinator", NodeID: "n1", Address: "n1:9001", Status: "ready", CreatedAt: now, LastSeen: now}
	for _, sh := range []store.Shard{coord,
		{ID: "p", ModelID: "big", GangID: "g0", Role: "rpc", NodeID: "n2", Address: "n2:50052", Status: "ready", CreatedAt: now, LastSeen: now},
	} {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	srv.gangSlotsAsked.Store("c", gangSlotsAnswer{addr: "n1:9001", n: 8})
	pool := int64(2048)
	srv.gangLoad.Store("c", nodeLoadSample{EngineLoad: engines.EngineLoad{KVCapacityTokens: &pool}, at: now})
	srv.plan.mu.Lock()
	srv.plan.context = 1024
	srv.plan.mu.Unlock()

	slots, ungoverned, _, kv := srv.slotsOf(ctx, "big")
	if slots != 2 || ungoverned != 0 || !kv {
		t.Fatalf("gang: slots %d ungoverned %d kv %v, want 2 0 true", slots, ungoverned, kv)
	}
	if n, ok := srv.routerSlots(router.GangKey("big", "g0")); !ok || n != 2 {
		t.Fatalf("the router reads the gang's bounded slots: %d %v", n, ok)
	}
	// No context: the coordinator's own 8.
	srv.plan.mu.Lock()
	srv.plan.context = 0
	srv.plan.mu.Unlock()
	if slots, _, _, kv := srv.slotsOf(ctx, "big"); slots != 8 || kv {
		t.Fatalf("no context: slots %d kv %v, want 8 false", slots, kv)
	}
}

// /loadz says the bound and carries the engines' own TTFT median; a leader
// whose admission is unbounded says no bound.
func TestLoadzSaysTheBoundAndTheEnginesTTFT(t *testing.T) {
	srv := slottedLeader(t, 8, PolicyAdmission{})
	srv.plan.mu.Lock()
	srv.plan.modelID, srv.plan.context = "m", 1024
	srv.plan.mu.Unlock()
	beatKV(t, srv, "w1", 8, 4096)
	srv.capacityChanged()

	get := func() adminapi.Load {
		rec := httptest.NewRecorder()
		srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
		var ld adminapi.Load
		if err := json.Unmarshal(rec.Body.Bytes(), &ld); err != nil {
			t.Fatal(err)
		}
		return ld
	}
	ld := get()
	if ld.AdmissionBound != "kv" {
		t.Fatalf("admission_bound = %q, want kv", ld.AdmissionBound)
	}
	if ld.EngineTTFTP50Ms != nil {
		t.Fatalf("no engine said a TTFT, got %d", *ld.EngineTTFTP50Ms)
	}

	// An ungoverned worker: admission is unbounded, no bound is said.
	beat(t, srv, "w1", 0)
	srv.capacityChanged()
	if ld := get(); ld.AdmissionBound != "" {
		t.Fatalf("an unbounded gate said %q", ld.AdmissionBound)
	}
}

func TestMedianStated(t *testing.T) {
	if medianStated(nil) != nil {
		t.Fatal("no figure, no median")
	}
	for _, c := range []struct {
		in   []int64
		want int64
	}{{[]int64{30}, 30}, {[]int64{30, 10, 20}, 20}, {[]int64{10, 31}, 21}} {
		if got := medianStated(c.in); got == nil || *got != c.want {
			t.Errorf("median %v = %v, want %d", c.in, got, c.want)
		}
	}
}

// The engines' TTFT figures fold into /loadz as their median, over the
// samples that state one.
func TestLoadzEngineTTFTIsTheWorkersMedian(t *testing.T) {
	srv := slottedLeader(t, 4, PolicyAdmission{})
	ctx := context.Background()
	now := time.Now()
	if err := srv.store.Nodes().Upsert(ctx, store.Node{ID: "w2", Hostname: "w2", State: "ready", Address: "127.0.0.1:2", WorkerToken: "wtok2", LastHeartbeat: now}); err != nil {
		t.Fatal(err)
	}
	beat(t, srv, "w2", 4)
	if err := srv.store.Nodes().Upsert(ctx, store.Node{ID: "w3", Hostname: "w3", State: "ready", Address: "127.0.0.1:3", WorkerToken: "wtok3", LastHeartbeat: now}); err != nil {
		t.Fatal(err)
	}
	beat(t, srv, "w3", 4)
	a, b := int64(10), int64(30)
	srv.nodeLoad.Store("w1", nodeLoadSample{EngineLoad: engines.EngineLoad{EngineTTFTP50Ms: &a}, at: now})
	srv.nodeLoad.Store("w2", nodeLoadSample{EngineLoad: engines.EngineLoad{EngineTTFTP50Ms: &b}, at: now})
	srv.nodeLoad.Store("w3", nodeLoadSample{EngineLoad: engines.EngineLoad{}, at: now}) // llama.cpp: says none
	var out adminapi.Load
	out.PlanModel = "m"
	srv.aggregateWorkerLoad(ctx, &out, now)
	if out.Reporting != 3 || out.EngineTTFTP50Ms == nil || *out.EngineTTFTP50Ms != 20 {
		t.Fatalf("reporting %d, engine TTFT %v: want 3 and the median of the two that say, 20", out.Reporting, out.EngineTTFTP50Ms)
	}
}

// The plan file's workload gives the endpoint's context: maxContext, else
// input + output; none, no context.
func TestPlanFileReadsTheWorkloadContext(t *testing.T) {
	for _, c := range []struct {
		workload string
		want     int
	}{
		{`"workload":{"inputTokens":2000,"outputTokens":500,"maxContext":4096},`, 4096},
		{`"workload":{"inputTokens":2000,"outputTokens":500},`, 2500},
		{``, 0},
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "plan.json")
		if err := os.WriteFile(path, []byte(`{"revision":3,`+c.workload+`"model":{"id":"m"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg := config.Default()
		cfg.Env.PlanFile = path
		st, err := store.OpenSQLite(":memory:")
		if err != nil {
			t.Fatal(err)
		}
		srv := NewServer(cfg, st, &downEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		ctx, cancel := context.WithCancel(context.Background())
		srv.StartPlanWatcher(ctx)
		got := srv.plan.endpointContext()
		cancel()
		_ = st.Close()
		if got != c.want {
			t.Errorf("workload %q: context %d, want %d", c.workload, got, c.want)
		}
	}
}
