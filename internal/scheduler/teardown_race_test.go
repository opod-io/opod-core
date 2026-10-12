package scheduler

// A teardown removes the gang it READ, never a gang formed again under the
// same id while it ran.
//
// Measured on the design-partner cell (2026-10-11): a worker reincarnated and
// its gang's teardown read g1 at 18:30:52, then waited 30 s on a stop dialled
// to a sibling part's dead pod address. Meanwhile a second teardown removed g1
// and a manager formed it again — new rows under the SAME ids, at 18:31:14. The
// first teardown's delete by gang at 18:31:22 removed the new g1, which went on
// running with no row to route it by.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// stopWorker answers every call; its FIRST stop blocks until released — a
// dial to an address that no longer answers.
type stopWorker struct {
	srv      *httptest.Server
	once     sync.Once
	blocked  chan struct{} // closed when the first stop is held
	release  chan struct{}
	holdStop bool
}

func newStopWorker(t *testing.T, hold bool) *stopWorker {
	t.Helper()
	w := &stopWorker{blocked: make(chan struct{}), release: make(chan struct{}), holdStop: hold}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.holdStop && strings.HasSuffix(r.URL.Path, "/v1/process/stop") {
			first := false
			w.once.Do(func() { first = true })
			if first {
				close(w.blocked)
				<-w.release
			}
		}
		_, _ = io.WriteString(rw, `[]`)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *stopWorker) addr() string { return strings.TrimPrefix(w.srv.URL, "http://") }

func TestASlowTeardownLeavesAGangFormedAgainUnderItsID(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	o.HTTP = &http.Client{Timeout: 10 * time.Second}

	fast, slow := newStopWorker(t, false), newStopWorker(t, true)
	for _, n := range []store.Node{
		{ID: "n1", Address: fast.addr(), WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now(), BootID: "b1"},
		{ID: "n2", Address: slow.addr(), WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now(), BootID: "b2"},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	part := func(name, node, addr string, created time.Time) store.Shard {
		role := "rpc"
		if name == "coord" {
			role = "coordinator"
		}
		return store.Shard{ID: gangShardID("m", "g1", name), ModelID: "m", GangID: "g1", Role: role, NodeID: node,
			Address: addr, ProcessID: gangShardID("m", "g1", name), Status: "ready", CreatedAt: created, LastSeen: created}
	}
	write := func(rows ...store.Shard) {
		t.Helper()
		for _, r := range rows {
			if err := st.Shards().Create(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The old g1: coordinator and one part on n1, one part on n2.
	old := time.Now().Add(-time.Minute)
	write(part("coord", "n1", "10.0.0.1:9001", old), part("rpc-0", "n1", "10.0.0.1:50052", old), part("rpc-1", "n2", "10.0.0.2:50053", old))

	// n1's worker reincarnated: teardown A reads g1 and stops its parts — and
	// the stop on n2 hangs.
	doneA := make(chan []store.GangKey, 1)
	go func() {
		removed, err := o.RemoveShardsOn(ctx, "n1")
		if err != nil {
			t.Error(err)
		}
		doneA <- removed
	}()
	select {
	case <-slow.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown A never reached the part on n2")
	}

	// Meanwhile n2's worker reincarnated too: teardown B removes the old g1 …
	if removed, err := o.RemoveShardsOn(ctx, "n2"); err != nil || len(removed) != 1 {
		t.Fatalf("teardown B removes the old g1: %v %v", removed, err)
	}
	// … and the gang is formed again under the same id, on new addresses.
	now := time.Now().Add(time.Second)
	fresh := []store.Shard{part("coord", "n1", "10.0.0.3:9001", now), part("rpc-0", "n1", "10.0.0.3:50052", now), part("rpc-1", "n2", "10.0.0.4:50053", now)}
	write(fresh...)

	close(slow.release) // teardown A's stop finally returns
	var removedA []store.GangKey
	select {
	case removedA = <-doneA:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown A never finished")
	}

	got, err := st.Shards().GetByGang(ctx, "m", "g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(fresh) {
		t.Fatalf("the g1 formed after teardown A read the old one must survive it: %d of %d rows left: %+v", len(got), len(fresh), got)
	}
	for _, r := range got {
		if !r.CreatedAt.Equal(time.Unix(now.Unix(), 0)) {
			t.Errorf("a row of the old g1 is back or the new one was rewritten: %+v", r)
		}
	}
	if len(removedA) != 0 {
		t.Errorf("teardown A removed nothing (B had), so it must not report the gang removed: %v", removedA)
	}
}
