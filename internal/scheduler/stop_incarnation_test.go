package scheduler

// A late stop reaches only the incarnation the teardown read.
//
// A gang teardown stops its parts one after another, and a stop dialled to a
// pod address that died with its worker can take 30 s. Its process ids are
// the gang's (gangShardID), so the same ids come back the moment the gang is
// formed again. A stop that was still queued behind the slow one then reached
// a SIBLING node by its current address and stopped the NEW gang's part with
// the same id — the gang read healthy and its coordinator lost a backend.

import (
	"context"
	"encoding/json"
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

// recordingWorker answers every call and remembers which processes it was
// asked to stop. With hold set, its first stop blocks until released.
type recordingWorker struct {
	srv     *httptest.Server
	hold    bool
	once    sync.Once
	blocked chan struct{}
	release chan struct{}

	mu      sync.Mutex
	stopped []string
}

func newRecordingWorker(t *testing.T, hold bool) *recordingWorker {
	t.Helper()
	w := &recordingWorker{hold: hold, blocked: make(chan struct{}), release: make(chan struct{})}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/process/stop") {
			var body struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			first := false
			if w.hold {
				w.once.Do(func() { first = true })
			}
			if first {
				close(w.blocked)
				<-w.release
			}
			w.mu.Lock()
			w.stopped = append(w.stopped, body.ID)
			w.mu.Unlock()
		}
		_, _ = io.WriteString(rw, `[]`)
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *recordingWorker) addr() string { return strings.TrimPrefix(w.srv.URL, "http://") }

func (w *recordingWorker) stops() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.stopped...)
}

func (w *recordingWorker) forget() {
	w.mu.Lock()
	w.stopped = nil
	w.mu.Unlock()
}

// lateStopFixture is a gang g1 of m with its coordinator on n1 (whose first
// stop hangs) and one part on n2.
func lateStopFixture(t *testing.T) (*Orchestrator, store.Store, *recordingWorker, *recordingWorker, []store.Shard, func(name, node, addr string, at time.Time) store.Shard) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	o.HTTP = &http.Client{Timeout: 10 * time.Second}
	slow, sibling := newRecordingWorker(t, true), newRecordingWorker(t, false)
	for _, n := range []store.Node{
		{ID: "n1", Address: slow.addr(), WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now(), BootID: "b1"},
		{ID: "n2", Address: sibling.addr(), WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now(), BootID: "b2"},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	part := func(name, node, addr string, at time.Time) store.Shard {
		role := "rpc"
		if name == "coord" {
			role = "coordinator"
		}
		return store.Shard{ID: gangShardID("m", "g1", name), ModelID: "m", GangID: "g1", Role: role, NodeID: node,
			Address: addr, ProcessID: gangShardID("m", "g1", name), Status: "ready", CreatedAt: at, LastSeen: at}
	}
	old := time.Now().Add(-time.Minute)
	rows := []store.Shard{part("coord", "n1", "10.0.0.1:9001", old), part("rpc-1", "n2", "10.0.0.2:50053", old)}
	for _, r := range rows {
		if err := st.Shards().Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	return o, st, slow, sibling, rows, part
}

// startSlowTeardown runs teardown A over rows and returns once its stop on n1
// is hanging; the returned channel closes when A finishes.
func startSlowTeardown(t *testing.T, o *Orchestrator, slow *recordingWorker, rows []store.Shard) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := o.removeGangRows(context.Background(), "m", rows); err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-slow.blocked:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown A never reached the part on n1")
	}
	return done
}

func finish(t *testing.T, slow *recordingWorker, done <-chan struct{}) {
	t.Helper()
	close(slow.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("teardown A never finished")
	}
}

func TestALateStopLeavesTheGangFormedAgainOnASiblingNode(t *testing.T) {
	ctx := context.Background()
	o, st, slow, sibling, rows, part := lateStopFixture(t)
	doneA := startSlowTeardown(t, o, slow, rows)

	// Meanwhile another teardown removes g1 (it stops n2's part itself) …
	if _, err := o.removeGangRows(ctx, "m", rows); err != nil {
		t.Fatal(err)
	}
	// … and the gang is formed again under the same ids, its part on n2 at
	// a new port. n2's worker now runs the NEW gang's s-m-g1-rpc-1.
	sibling.forget()
	at := time.Now().Add(time.Second)
	for _, r := range []store.Shard{part("coord", "n1", "10.0.0.3:9001", at), part("rpc-1", "n2", "10.0.0.2:50054", at)} {
		if err := st.Shards().Create(ctx, r); err != nil {
			t.Fatal(err)
		}
	}

	finish(t, slow, doneA)
	if got := sibling.stops(); len(got) != 0 {
		t.Fatalf("teardown A's late stop reached the gang formed after it read the old one: stopped %v on n2", got)
	}
}

func TestALateStopSkipsAWorkerReplacedSinceTheRead(t *testing.T) {
	ctx := context.Background()
	o, st, slow, sibling, rows, _ := lateStopFixture(t)
	doneA := startSlowTeardown(t, o, slow, rows)

	// n2's worker is replaced (a recreated pod): the part's process went with
	// the old one, and the new one at n2's address is not what A read.
	n2, err := st.Nodes().Get(ctx, "n2")
	if err != nil || n2 == nil {
		t.Fatalf("node n2: %v", err)
	}
	n2.BootID = "b2-new"
	if err := st.Nodes().Upsert(ctx, *n2); err != nil {
		t.Fatal(err)
	}

	finish(t, slow, doneA)
	if got := sibling.stops(); len(got) != 0 {
		t.Fatalf("a stop must target the worker incarnation the teardown read, not the one there now: stopped %v on n2", got)
	}
	if left, _ := st.Shards().GetByGang(ctx, "m", "g1"); len(left) != 0 {
		t.Fatalf("the rows as read still go: %+v", left)
	}

	// Control: with nothing changed since the read, every part is stopped.
	o2, _, slow2, sibling2, rows2, _ := lateStopFixture(t)
	finish(t, slow2, startSlowTeardown(t, o2, slow2, rows2))
	if got := sibling2.stops(); len(got) != 1 || got[0] != rows2[1].ProcessID {
		t.Fatalf("an unchanged part must be stopped: %v", got)
	}
}
