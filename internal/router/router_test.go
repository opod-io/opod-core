package router

import (
	"sync"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/all" // getOrCreateRemote resolves drivers via the registry
)

// TestGetOrCreateRemote_NoDuplicates is a regression test for the
// TOCTOU window that previously allowed two concurrent callers to each
// construct a remote engine for the same nodeID. Run with `-race` and
// many parallel callers; only one engine should end up cached and all
// callers should observe the same pointer.
func TestGetOrCreateRemote_NoDuplicates(t *testing.T) {
	r := &Router{
		inflight: make(map[string]int),
		remotes:  make(map[string]engines.Engine),
	}

	const goroutines = 64
	var wg sync.WaitGroup
	results := make([]engines.Engine, goroutines)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer wg.Done()
			results[i] = r.getOrCreateRemote("node-A", "worker.local:7000", "tok")
		}()
	}
	wg.Wait()

	if len(r.remotes) != 1 {
		t.Fatalf("expected exactly 1 remote cached, got %d", len(r.remotes))
	}
	first := results[0]
	if first == nil {
		t.Fatal("first result is nil")
	}
	for i, e := range results {
		if e == nil {
			t.Errorf("result[%d] is nil", i)
		}
		if e != first {
			t.Errorf("result[%d] is a different engine pointer than result[0] — race regression", i)
		}
	}
}

// TestGetOrCreateRemote_DifferentNodes confirms that different nodeIDs
// get distinct engines, so the race fix didn't accidentally collapse
// them.
func TestGetOrCreateRemote_DifferentNodes(t *testing.T) {
	r := &Router{
		inflight: make(map[string]int),
		remotes:  make(map[string]engines.Engine),
	}
	a := r.getOrCreateRemote("node-A", "a.local:7000", "")
	b := r.getOrCreateRemote("node-B", "b.local:7000", "")
	if a == b {
		t.Fatal("different nodes returned same engine pointer")
	}
	if len(r.remotes) != 2 {
		t.Errorf("expected 2 cached remotes, got %d", len(r.remotes))
	}
}
