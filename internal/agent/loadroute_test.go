package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/opod-io/opod-core/internal/engines"
)

type countingLoad struct {
	engines.Engine
	calls atomic.Int64
}

func (c *countingLoad) Name() string { return "vllm" }
func (c *countingLoad) Load(context.Context) (engines.EngineLoad, error) {
	c.calls.Add(1)
	return engines.EngineLoad{KVUsedPct: 42, QueueDepth: 3}, nil
}

// ADR-109: N routers pulling at once cost the engine one scrape per interval.
func TestLoadRouteScrapesTheEngineOncePerInterval(t *testing.T) {
	eng := &countingLoad{}
	srv := &Server{Engine: eng}
	get := func() engines.EngineLoad {
		rec := httptest.NewRecorder()
		srv.load(rec, httptest.NewRequest(http.MethodGet, "/v1/load?maxAgeMs=60000", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		var ld engines.EngineLoad
		if err := json.Unmarshal(rec.Body.Bytes(), &ld); err != nil {
			t.Fatal(err)
		}
		return ld
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); get() }()
	}
	wg.Wait()
	if n := eng.calls.Load(); n != 1 {
		t.Fatalf("20 concurrent pulls inside one interval scraped the engine %d times, want 1", n)
	}
	if ld := get(); ld.KVUsedPct != 42 || ld.QueueDepth != 3 || ld.SampledAtMs == 0 {
		t.Fatalf("sample %+v: want the engine's figures, dated in ms", ld)
	}
}

// An engine that reports no load is refused by name, so the puller leaves
// the worker on its heartbeat.
func TestLoadRouteOnAnEngineWithoutLoadIs501(t *testing.T) {
	srv := &Server{Engine: &listStub{}}
	rec := httptest.NewRecorder()
	srv.load(rec, httptest.NewRequest(http.MethodGet, "/v1/load", nil))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501, got %d", rec.Code)
	}
}
