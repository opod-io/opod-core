package leader

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/store"
	"github.com/opod-io/opod-core/pkg/nodeapi"
)

// ADR-109: the picker ranks on a pulled sample while it is fresh, and a
// pulled sample older than three intervals ranks nothing.
func TestAPulledSampleOlderThanThreeIntervalsIsNotRanked(t *testing.T) {
	srv, _, _ := drainFixture(t)
	srv.loadRefreshMs.Store(500)
	now := time.Now()
	srv.pull.samples["w1"] = nodeLoadSample{EngineLoad: engines.EngineLoad{KVUsedPct: 90, QueueDepth: 7}, at: now.Add(-time.Second)}
	if sig, ok := srv.loadSignal("w1"); !ok || sig.QueueDepth != 7 || sig.SampledAt.IsZero() {
		t.Fatalf("a 1 s old sample at a 500 ms interval: %+v ok=%v, want it ranked and dated", sig, ok)
	}
	srv.pull.samples["w1"] = nodeLoadSample{EngineLoad: engines.EngineLoad{KVUsedPct: 90, QueueDepth: 7}, at: now.Add(-1501 * time.Millisecond)}
	// A fresh heartbeat sample must not stand in: the pull is the truth for a pulled worker.
	srv.nodeLoad.Store("w1", nodeLoadSample{EngineLoad: engines.EngineLoad{QueueDepth: 1}, at: now})
	if _, ok := srv.loadSignal("w1"); ok {
		t.Fatal("a pulled sample 3 × 500 ms + 1 ms old was ranked")
	}
}

// A worker whose core has no /v1/load keeps its heartbeat's sample and that
// sample's 30 s rule: an older worker does not lose load-aware routing.
func TestAWorkerThatCannotBePulledKeepsItsHeartbeatSample(t *testing.T) {
	srv, _, _ := drainFixture(t)
	srv.pull.unsupported["w2"] = time.Now()
	srv.nodeLoad.Store("w2", nodeLoadSample{EngineLoad: engines.EngineLoad{QueueDepth: 4}, at: time.Now().Add(-4 * time.Second)})
	if sig, ok := srv.loadSignal("w2"); !ok || sig.QueueDepth != 4 {
		t.Fatalf("heartbeat sample of an unpullable worker: %+v ok=%v", sig, ok)
	}
}

// The puller GETs /v1/load signed, stores the worker's sample dated by the
// worker's own clock, and a 404 marks the worker unpullable.
func TestThePullerFetchesASignedSampleAndMarksA404(t *testing.T) {
	srv, _, _ := drainFixture(t)
	stamp := time.Now().Add(-200 * time.Millisecond).UnixMilli()
	var gotAuth, gotQuery string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeapi.PathLoad {
			http.NotFound(w, r)
			return
		}
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(engines.EngineLoad{KVUsedPct: 55, QueueDepth: 2, SampledAtMs: stamp})
	}))
	defer worker.Close()
	n := &store.Node{ID: "wp", Address: worker.URL, WorkerToken: "tok"}
	srv.pull.begin("wp", time.Now())
	srv.pullLoad(context.Background(), n, 500*time.Millisecond)
	smp, pulls, fresh := srv.pulledLoad("wp", time.Now())
	if !pulls || !fresh || smp.QueueDepth != 2 || smp.at.UnixMilli() != stamp {
		t.Fatalf("pulled %+v pulls=%v fresh=%v", smp, pulls, fresh)
	}
	if gotAuth != "Bearer tok" || !strings.Contains(gotQuery, "maxAgeMs=500") {
		t.Fatalf("request auth %q query %q", gotAuth, gotQuery)
	}

	old := httptest.NewServer(http.NotFoundHandler())
	defer old.Close()
	srv.pull.begin("wo", time.Now())
	srv.pullLoad(context.Background(), &store.Node{ID: "wo", Address: old.URL, WorkerToken: "tok"}, 500*time.Millisecond)
	if _, pulls, _ := srv.pulledLoad("wo", time.Now()); pulls {
		t.Fatal("a 404 left a pulled sample")
	}
	if srv.pull.begin("wo", time.Now()) {
		t.Fatal("a worker that answered 404 is asked again at once")
	}
}

// A door ranks on samples it pulls itself: it gets none from heartbeats (only
// the leader's heartbeat handler stores one), so before ADR-109 it ranked on
// its own in-flight count alone. Two workers with different KV use; the door
// pulls both, ranks each on its own figure, and /loadz says so.
func TestADoorPullsAndRanksOnEachWorkersLoad(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	workerAt := func(kv float64) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != nodeapi.PathLoad {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(engines.EngineLoad{KVUsedPct: kv, SampledAtMs: time.Now().UnixMilli()})
		}))
	}
	busy, idle := workerAt(92), workerAt(8)
	defer busy.Close()
	defer idle.Close()
	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }

	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/admin/v1/shards":
			_, _ = w.Write([]byte(`[]`))
		case "/admin/v1/spend":
			_, _ = w.Write([]byte(`{"ts":0,"doors":1,"lag_bound_ms":10000,"keys":[]}`))
		default:
			_, _ = fmt.Fprintf(w, `[{"ID":"busy","Hostname":"busy","Address":%q,"state":"ready","heartbeat_age_seconds":1,"WorkerToken":"sk-orc-b"},`+
				`{"ID":"idle","Hostname":"idle","Address":%q,"state":"ready","heartbeat_age_seconds":1,"WorkerToken":"sk-orc-i"}]`, host(busy), host(idle))
		}
	}))
	defer leader.Close()

	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Env.Role = "gateway"
	cfg.Env.LeaderURL = leader.URL
	cfg.Env.GatewayID = "door-1"
	cfg.Auth.AdminToken = "sk-orc-contract-test-admin"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	door := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, nil)
	if err := door.StartGatewayRole(ctx); err != nil {
		t.Fatalf("StartGatewayRole: %v", err)
	}
	door.loadRefreshMs.Store(100)
	door.StartLoadPuller(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		b, okB := door.loadSignal("busy")
		i, okI := door.loadSignal("idle")
		if okB && okI {
			if b.KVUsedPct != 92 || i.KVUsedPct != 8 {
				t.Fatalf("door ranks busy on KV %.0f and idle on %.0f, want 92 and 8", b.KVUsedPct, i.KVUsedPct)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the door pulled no sample in 5 s (busy ok=%v, idle ok=%v)", okB, okI)
		}
		time.Sleep(20 * time.Millisecond)
	}

	rec := httptest.NewRecorder()
	door.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
	var lz struct {
		LoadSamples map[string]struct {
			Source string `json:"source"`
			Ranked bool   `json:"ranked"`
		} `json:"load_samples"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lz); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"busy", "idle"} {
		if smp := lz.LoadSamples[id]; smp.Source != "pull" || !smp.Ranked {
			t.Errorf("/loadz load_samples[%s] = %+v, want a ranked pull", id, smp)
		}
	}
}
