package leader

// T22.4: a gang part whose process is down — it could not start again, its
// supervisor ran out of restarts — must take its gang's rows with it, so the
// heal forms the gang again (and gives up after three) and a manager reading
// the rows does not see a serving gang.
//
// Measured on the design-partner cell (2026-10-18): a part whose process could
// not be started again left its worker heartbeating, its rows "ready", the
// router refusing every request for the model, and the heal idle — the gang
// was present, so there was nothing to heal — for as long as anyone watched.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/nodeapi"
	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// partWorker answers the process list with the part's process in the status
// the test sets, and counts the lists it was asked for.
type partWorker struct {
	mu     sync.Mutex
	status string // the part's process status; "" = not listed at all
	lists  atomic.Int32
	srv    *httptest.Server
}

func newPartWorker(t *testing.T, status string) *partWorker {
	t.Helper()
	w := &partWorker{status: status}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/process/list"):
			w.lists.Add(1)
			w.mu.Lock()
			st := w.status
			w.mu.Unlock()
			procs := []agent.ProcessInfo{}
			if st != "" {
				procs = append(procs, agent.ProcessInfo{ID: "p-rpc", Status: st})
			}
			_ = json.NewEncoder(rw).Encode(procs)
		default:
			rw.WriteHeader(http.StatusOK) // stop
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func partDownFixture(t *testing.T, status string) (*Server, store.Store, *partWorker) {
	t.Helper()
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := scheduler.New(st, nil, log, "")
	orch.HTTP = &http.Client{Timeout: 2 * time.Second}
	cfg := config.Default()
	cfg.Listen = ":0"
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, orch)
	w := newPartWorker(t, status)
	addr := strings.TrimPrefix(w.srv.URL, "http://")
	if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: "n1", Address: addr, BootID: "boot-a"}, Caller{Admin: true}, "tok"); err != nil {
		t.Fatal(err)
	}
	for _, sh := range []store.Shard{
		{ID: "s-coord", ModelID: "m", Role: "coordinator", NodeID: "elsewhere", Address: "10.0.0.9:9001", ProcessID: "p-coord", Status: "ready"},
		{ID: "s-rpc", ModelID: "m", Role: "rpc", NodeID: "n1", Address: addr, ProcessID: "p-rpc", Status: "ready"},
	} {
		sh.CreatedAt, sh.LastSeen = time.Now(), time.Now()
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	return srv, st, w
}

func heartbeatEngine(t *testing.T, srv *Server, state string) {
	t.Helper()
	hb := HeartbeatRequest{ID: "n1", BootID: "boot-a", LoadedModels: []string{}, Engine: &nodeapi.EngineState{State: state}}
	if err := srv.HeartbeatNode(context.Background(), hb, Caller{Admin: true}); err != nil {
		t.Fatal(err)
	}
}

func TestAPartWhoseProcessIsDownTakesItsGangWithIt(t *testing.T) {
	ctx := context.Background()
	srv, st, w := partDownFixture(t, "crashloop") // the supervisor ran out of restarts
	heartbeatEngine(t, srv, nodeapi.EngineCrashLooping)

	deadline := time.Now().Add(3 * time.Second)
	for {
		rows, _ := st.Shards().GetByModel(ctx, "m")
		if len(rows) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the gang whose part's process is down must be removed, still %d rows", len(rows))
		}
		time.Sleep(20 * time.Millisecond)
	}
	events, _ := st.EventLog().After(ctx, 0, 100)
	found := false
	for _, e := range events {
		if e.Type == "shard.stale" && strings.Contains(fmt.Sprint(e.Data["reason"]), "part's process down") {
			found = true
		}
	}
	if !found {
		t.Fatalf("shard.stale must say the part's process is down: %+v", events)
	}
	if w.lists.Load() == 0 {
		t.Fatal("the gang must go on the worker's own process list, not on the report alone")
	}
}

func TestAPartThatIsRunningAgainKeepsItsGang(t *testing.T) {
	ctx := context.Background()
	// Restarted more than once, up now: the worker reports crash-looping,
	// and its process list says the part runs.
	srv, st, w := partDownFixture(t, "running")
	heartbeatEngine(t, srv, nodeapi.EngineCrashLooping)
	deadline := time.Now().Add(3 * time.Second)
	for w.lists.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if w.lists.Load() == 0 {
		t.Fatal("the report must ask the process list")
	}
	time.Sleep(100 * time.Millisecond) // the removal, were there one, runs right after the list
	if rows, _ := st.Shards().GetByModel(ctx, "m"); len(rows) != 2 {
		t.Fatalf("a part that is running keeps its gang: %d rows", len(rows))
	}

	// The same report again is not a change: no second list.
	asked := w.lists.Load()
	heartbeatEngine(t, srv, nodeapi.EngineCrashLooping)
	time.Sleep(100 * time.Millisecond)
	if got := w.lists.Load(); got != asked {
		t.Fatalf("an unchanged report must not ask again: %d lists, want %d", got, asked)
	}
	// Serving, then stopped: a change into a down state asks again.
	heartbeatEngine(t, srv, nodeapi.EngineServing)
	heartbeatEngine(t, srv, nodeapi.EngineStopped)
	deadline = time.Now().Add(3 * time.Second)
	for w.lists.Load() == asked && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if w.lists.Load() == asked {
		t.Fatal("a part that stopped must be checked")
	}
}
