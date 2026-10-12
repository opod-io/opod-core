package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// scriptedWorker answers /v1/process/get from a script the test changes as it
// goes: a status (with optional exit text), or an HTTP error.
type scriptedWorker struct {
	mu      sync.Mutex
	gets    int
	answer  func(n int) (status, exitErr string, code int) // n = 1-based get count
	started chan struct{}
	srv     *httptest.Server
}

func newScriptedWorker(t *testing.T, answer func(n int) (string, string, int)) *scriptedWorker {
	t.Helper()
	w := &scriptedWorker{answer: answer, started: make(chan struct{}, 4)}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && strings.HasSuffix(r.URL.Path, "/v1/process/file"):
			rw.Header().Set("X-File-Path", "/models/m.gguf")
			rw.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/v1/process/start"):
			var body struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_ = json.NewEncoder(rw).Encode(map[string]any{"id": body.ID, "status": "starting"})
			w.started <- struct{}{}
		case strings.HasSuffix(r.URL.Path, "/v1/process/get"):
			w.mu.Lock()
			w.gets++
			status, exitErr, code := w.answer(w.gets)
			w.mu.Unlock()
			if code != 0 {
				http.Error(rw, "worker unavailable", code)
				return
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{"id": r.URL.Query().Get("id"), "status": status, "exit_err": exitErr, "restarts": 1})
		default:
			_, _ = io.WriteString(rw, `[]`)
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func partLostFixture(t *testing.T, w *scriptedWorker) (*Orchestrator, models.Entry) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	n := store.Node{
		ID: "n1", Address: strings.TrimPrefix(w.srv.URL, "http://"), WorkerToken: "tok",
		State: store.NodeStateReady, LastHeartbeat: time.Now(), RAMGB: 64, BootID: "boot-a",
	}
	if err := st.Nodes().Upsert(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	gguf := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(gguf, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), "")
	o.HTTP = &http.Client{Timeout: 2 * time.Second}
	o.pollEvery = 10 * time.Millisecond
	entry := models.Entry{
		ID:       "m-sharded",
		Source:   models.SourceSpec{Type: "file", Path: gguf},
		Sharding: models.ShardingSpec{Required: true, DefaultShards: 1},
	}
	return o, entry
}

func createIn(o *Orchestrator, entry models.Entry) <-chan error {
	done := make(chan error, 1)
	go func() { done <- o.CreateSharded(context.Background(), entry, "g1", 1, []string{"n1"}, Parallelism{}) }()
	return done
}

func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the create was still forming after 10 s")
		return nil
	}
}

// A worker that is still the same process and briefly fails its answers is a
// blip: the create waits it out and forms.
func TestABriefSameIncarnationErrorIsTolerated(t *testing.T) {
	w := newScriptedWorker(t, func(n int) (string, string, int) {
		switch {
		case n <= 3:
			return "", "", http.StatusBadGateway
		case n <= 5:
			return "starting", "", 0
		}
		return "running", "", 0
	})
	o, entry := partLostFixture(t, w)
	if err := waitDone(t, createIn(o, entry)); err != nil {
		t.Fatalf("a brief blip on the same worker process must not fail the create: %v", err)
	}
	shards, _ := o.Store.Shards().GetByGang(context.Background(), entry.ID, "g1")
	if len(shards) != 1 || shards[0].Role != "coordinator" {
		t.Fatalf("the gang formed with its coordinator recorded, got %v", shards)
	}
}

// With no notification at all, a failed poll reads the node's row again: a
// new boot id there means the part's process is gone. The engine's last lines
// the leader polled before it went are in the error.
func TestAFailedPollOnAReplacedWorkerEndsTheCreate(t *testing.T) {
	var replaced sync.Once
	var o *Orchestrator
	w := newScriptedWorker(t, func(n int) (string, string, int) {
		if n <= 2 {
			return "starting", "exit status 1\nllama_init_from_model: failed to allocate KV cache", 0
		}
		replaced.Do(func() {
			// The pod was recreated: a new process registered under the id.
			go func() {
				nd, _ := o.Store.Nodes().Get(context.Background(), "n1")
				nd.BootID = "boot-b"
				_ = o.Store.Nodes().Upsert(context.Background(), *nd)
			}()
		})
		return "", "", http.StatusNotFound
	})
	o, entry := partLostFixture(t, w)
	err := waitDone(t, createIn(o, entry))
	if !errors.Is(err, ErrPartLost) {
		t.Fatalf("the create must end with ErrPartLost: %v", err)
	}
	for _, want := range []string{"gang g1", "n1", "replaced", "failed to allocate KV cache"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must carry %q: %q", want, err)
		}
	}
	// The guard is free at once.
	release, cerr := o.forms.claim(entry.ID, "g1")
	if cerr != nil {
		t.Fatalf("the guard must be released when the create ends: %v", cerr)
	}
	release()
}

// News about a node the create does not use changes nothing for it.
func TestALostWorkerElsewhereDoesNotEndTheCreate(t *testing.T) {
	gate := make(chan struct{})
	w := newScriptedWorker(t, func(n int) (string, string, int) {
		select {
		case <-gate:
			return "running", "", 0
		default:
			return "starting", "", 0
		}
	})
	o, entry := partLostFixture(t, w)
	done := createIn(o, entry)
	<-w.started
	o.EndFormationsOn("n9")
	o.endFormationsOn("n9", lostReplaced)
	close(gate)
	if err := waitDone(t, done); err != nil {
		t.Fatalf("a create on n1 must not end for news about n9: %v", err)
	}
}

// The notification ends the create while it waits, and names the node.
func TestANotifiedLossEndsTheCreate(t *testing.T) {
	w := newScriptedWorker(t, func(int) (string, string, int) { return "starting", "", 0 })
	o, entry := partLostFixture(t, w)
	done := createIn(o, entry)
	<-w.started
	time.Sleep(50 * time.Millisecond) // a few polls: the last status is known
	began := time.Now()
	o.endFormationsOn("n1", lostReplaced)
	err := waitDone(t, done)
	if took := time.Since(began); took > time.Second {
		t.Errorf("the create took %s to end after the leader knew", took)
	}
	var pl *PartLostError
	if !errors.As(err, &pl) || pl.Gang != "g1" || pl.Node != "n1" || pl.Last == nil || pl.Last.Status != "starting" {
		t.Fatalf("want a PartLostError for g1 on n1 with the last status polled, got %#v (%v)", pl, err)
	}
	if !strings.Contains(err.Error(), "reported no output") {
		t.Errorf("with no exit text, the error says no output was reported: %q", err)
	}
}
