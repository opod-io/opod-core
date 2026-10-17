package scheduler

// Acquire/release pairing (T22.4): a start that does not end ready stops what
// it started, whichever backend asked for it; a placement that fails part-way
// unloads what it had loaded.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

type pathWorker struct {
	mu     sync.Mutex
	calls  []string
	status func(path string) (int, string)
}

func (f *pathWorker) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.URL.Path)
		f.mu.Unlock()
		code, body := f.status(r.URL.Path)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	})
}

func (f *pathWorker) count(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == path {
			n++
		}
	}
	return n
}

func giveBackOrch(t *testing.T, workers map[string]*pathWorker) *Orchestrator {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for id, fw := range workers {
		srv := httptest.NewServer(fw.handler())
		t.Cleanup(srv.Close)
		if err := st.Nodes().Upsert(context.Background(), store.Node{ID: id, Address: strings.TrimPrefix(srv.URL, "http://"),
			WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	o.pollEvery = 10 * time.Millisecond
	return o
}

// A part that fails while it starts, or never becomes ready, is stopped by the
// start itself — its caller's rollback only knew the parts that had finished.
// A start the worker refused started nothing and is not stopped.
func TestAStartThatDoesNotEndReadyStopsWhatItStarted(t *testing.T) {
	for name, tc := range map[string]struct {
		get     string
		refused bool
		stops   int
	}{
		"failed":      {get: `{"status":"failed","exit_err":"CUDA out of memory"}`, stops: 1},
		"never-ready": {get: `{"status":"starting"}`, stops: 1},
		"refused":     {refused: true, stops: 0},
	} {
		t.Run(name, func(t *testing.T) {
			fw := &pathWorker{status: func(path string) (int, string) {
				switch path {
				case "/v1/process/start":
					if tc.refused {
						return 409, `{"error":"exists"}`
					}
					return 202, `{"status":"starting"}`
				case "/v1/process/get":
					return 200, tc.get
				}
				return 200, `{}`
			}}
			o := giveBackOrch(t, map[string]*pathWorker{"w1": fw})
			node, _ := o.Store.Nodes().Get(context.Background(), "w1")
			_, err := o.callWorkerStart(context.Background(), *node, agent.ProcessSpec{ID: "shard-m-0", Command: "rpc-server", ReadyTimeout: 100 * time.Millisecond})
			if err == nil {
				t.Fatal("the start must fail")
			}
			if got := fw.count("/v1/process/stop"); got != tc.stops {
				t.Fatalf("stops %d, want %d (err %v)", got, tc.stops, err)
			}
			var pf *PartFailedError
			if name == "failed" && !errors.As(err, &pf) {
				t.Fatalf("a failed part keeps its definite error: %v", err)
			}
		})
	}
}

// A placement on several nodes that fails on one unloads what it loaded on the
// nodes before it.
func TestAPlacementThatFailsPartWayUnloadsWhatItLoaded(t *testing.T) {
	ok := &pathWorker{status: func(path string) (int, string) {
		if path == "/v1/model/unload" {
			return 200, `{"status":"unloaded","model":"example/M"}`
		}
		return 200, `{"status":"ready"}`
	}}
	bad := &pathWorker{status: func(string) (int, string) { return 500, `{"error":"no space left"}` }}
	o := giveBackOrch(t, map[string]*pathWorker{"w1": ok, "w2": bad})
	entry := models.Entry{ID: "m", Source: models.SourceSpec{Type: "huggingface", Repo: "example/M"}}
	err := o.PlaceOnNodes(context.Background(), entry, []string{"w1", "w2"}, true, true)
	if err == nil || !strings.Contains(err.Error(), "node w2") {
		t.Fatalf("the failing node is named: %v", err)
	}
	if ok.count("/v1/model/load") != 1 || ok.count("/v1/model/unload") != 1 {
		t.Fatalf("w1 loaded once and was unloaded once: %v", ok.calls)
	}
}
