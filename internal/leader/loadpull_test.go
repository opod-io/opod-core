package leader

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
