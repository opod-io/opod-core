package leader

// A gang create whose part's worker is replaced while the gang forms must end
// at once, with a reason, and free its guard — not poll the dead process for
// the whole formation budget while the heal loop is refused.
//
// Measured on the design-partner cell (2026-10-11): a coordinator's worker pod
// died 17 s after the coordinator started, the leader logged "gang removed: its
// part died with the worker's previous incarnation" the same second, and the
// create went on polling the dead process for 7.5 more minutes — refusing every
// heal attempt with "a create is already forming this gang" — until its
// caller's own deadline cancelled it with no reason at all.

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

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// formingWorker answers like a worker whose coordinator never leaves
// "starting" — a llama-server still loading — until the process is gone.
type formingWorker struct {
	mu      sync.Mutex
	started chan string // the process id of each start, as it lands
	gone    bool        // the process died with the worker's previous incarnation
	crashed string      // non-empty: the process reports "crashloop" with these last lines
	srv     *httptest.Server
}

func newFormingWorker(t *testing.T) *formingWorker {
	t.Helper()
	w := &formingWorker{started: make(chan string, 4)}
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
			w.started <- body.ID
		case strings.HasSuffix(r.URL.Path, "/v1/process/get"):
			w.mu.Lock()
			gone, crashed := w.gone, w.crashed
			w.mu.Unlock()
			if gone {
				http.Error(rw, `process "`+r.URL.Query().Get("id")+`" not found`, http.StatusNotFound)
				return
			}
			if crashed != "" {
				_ = json.NewEncoder(rw).Encode(map[string]any{"id": r.URL.Query().Get("id"), "status": "crashloop", "restarts": 5, "exit_err": crashed})
				return
			}
			_ = json.NewEncoder(rw).Encode(map[string]any{"id": r.URL.Query().Get("id"), "status": "starting"})
		default:
			_, _ = io.WriteString(rw, `[]`) // process list, stop
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func formationFixture(t *testing.T) (*Server, *scheduler.Orchestrator, *formingWorker, models.Entry) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	gguf := filepath.Join(t.TempDir(), "m.gguf")
	if err := os.WriteFile(gguf, []byte("gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	entry := models.Entry{
		ID:       "m-sharded",
		Source:   models.SourceSpec{Type: "file", Path: gguf},
		Sharding: models.ShardingSpec{Required: true, DefaultShards: 1},
	}
	orch := scheduler.New(st, nil, log, "")
	orch.HTTP = &http.Client{Timeout: 2 * time.Second}
	cfg := config.Default()
	cfg.Listen = ":0"
	srv := NewServer(cfg, st, &stubLeaderEngine{}, []models.Entry{entry}, log, orch)
	w := newFormingWorker(t)
	addr := strings.TrimPrefix(w.srv.URL, "http://")
	if _, err := srv.RegisterNode(context.Background(), RegisterRequest{ID: "n1", Address: addr, BootID: "boot-a"}, Caller{Admin: true}, "tok"); err != nil {
		t.Fatal(err)
	}
	return srv, orch, w, entry
}

// awaitCreate returns the create's error, or fails the test when it is still
// running after the bound — without the fix it would poll for 15 minutes.
func awaitCreate(t *testing.T, done <-chan error, bound time.Duration) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(bound):
		t.Fatalf("the create was still forming %s after its part's worker went away", bound)
		return nil
	}
}

func TestACreateEndsWhenItsPartsWorkerIsReplaced(t *testing.T) {
	srv, orch, w, entry := formationFixture(t)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() {
		done <- srv.CreateShards(ctx, CreateShardsRequest{ModelID: entry.ID, Gang: "g1", Shards: 1, Nodes: []string{"n1"}})
	}()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the coordinator was never started")
	}

	// The worker's pod is recreated: the old process (and the coordinator it
	// was starting) is gone, and a new one announces itself under the same id.
	w.mu.Lock()
	w.gone = true
	w.mu.Unlock()
	began := time.Now()
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "n1", BootID: "boot-b"}, Caller{Admin: true}); err != nil {
		t.Fatal(err)
	}

	err := awaitCreate(t, done, 10*time.Second)
	if err == nil {
		t.Fatal("a create whose part died with its worker must fail")
	}
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("the create took %s to notice; the leader knew at once", took)
	}
	if !errors.Is(err, scheduler.ErrPartLost) {
		t.Errorf("the error must say a part's worker went away: %v", err)
	}
	if errors.Is(err, scheduler.ErrCreateInFlight) {
		t.Errorf("the error must carry the reason, not the guard's refusal: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"gang g1", "n1", "replaced", "reported no output"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error must name %q so a person can act on it: %q", want, msg)
		}
	}

	// The guard is free: the heal loop can form the gang again at once. A
	// second create gets as far as starting its coordinator (it is not refused
	// with ErrCreateInFlight).
	w.mu.Lock()
	w.gone = false
	w.mu.Unlock()
	again := make(chan error, 1)
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		again <- orch.CreateSharded(cctx, entry, "g1", 1, []string{"n1"}, scheduler.Parallelism{})
	}()
	select {
	case err := <-again:
		t.Fatalf("a create right after the failed one must not be refused: %v", err)
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the next create never started its coordinator")
	}
	cancel()
	<-again
}

// A removed node ends the create the same way: the operator said it is gone.
func TestACreateEndsWhenItsPartsNodeIsRemoved(t *testing.T) {
	srv, _, w, entry := formationFixture(t)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- srv.CreateShards(ctx, CreateShardsRequest{ModelID: entry.ID, Gang: "g1", Shards: 1, Nodes: []string{"n1"}})
	}()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the coordinator was never started")
	}
	if err := srv.RemoveNode(ctx, "n1"); err != nil {
		t.Fatal(err)
	}
	err := awaitCreate(t, done, 10*time.Second)
	if !errors.Is(err, scheduler.ErrPartLost) || !strings.Contains(err.Error(), "removed") || !strings.Contains(err.Error(), "gang g1") {
		t.Fatalf("a create whose node was removed must end naming the gang and why: %v", err)
	}
}

// The create's HTTP answer says which of the two it is in error.code (feature
// shard_create_error_codes): a definite "this gang did not form" a manager
// stops waiting on, or a collision with a running create it retries. On the
// cell (2026-10-11) the manager could only read the English, kept retrying the
// definite answer, collided with the leader's own heal create eleven times,
// and failed eight minutes later carrying the collision's text.
func TestAFailedShardCreateNamesItsCauseInTheCode(t *testing.T) {
	srv, _, w, entry := formationFixture(t)
	ctx := context.Background()
	body := `{"model_id":"` + entry.ID + `","gang":"g1","shards":1,"nodes":["n1"]}`
	post := func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.createShards(rec, httptest.NewRequest(http.MethodPost, "/admin/v1/shards/create", strings.NewReader(body)))
		return rec
	}
	type apiErr struct {
		Error struct {
			Message, Type, Code string
		} `json:"error"`
	}
	read := func(rec *httptest.ResponseRecorder) apiErr {
		t.Helper()
		var e apiErr
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
			t.Fatalf("the error body must be JSON: %v: %s", err, rec.Body.String())
		}
		return e
	}

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- post() }()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the coordinator was never started")
	}

	// A second create of the same gang while the first forms: refused, and
	// the code says it was a collision — not an answer about the gang.
	busy := post()
	if busy.Code != http.StatusBadGateway {
		t.Fatalf("the collision keeps its status: %d %s", busy.Code, busy.Body.String())
	}
	if e := read(busy); e.Error.Code != ShardCreateInFlight || e.Error.Type != "server_error" {
		t.Fatalf("a collision must carry code %q (type unchanged): %+v", ShardCreateInFlight, e.Error)
	}

	// The first create's part's worker is replaced: a definite answer.
	w.mu.Lock()
	w.gone = true
	w.mu.Unlock()
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "n1", BootID: "boot-b"}, Caller{Admin: true}); err != nil {
		t.Fatal(err)
	}
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("the create was still forming after its part's worker went away")
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("the definite answer keeps its status: %d %s", rec.Code, rec.Body.String())
	}
	e := read(rec)
	if e.Error.Code != ShardCreatePartLost || e.Error.Type != "server_error" {
		t.Fatalf("a gang that did not form must carry code %q (type unchanged): %+v", ShardCreatePartLost, e.Error)
	}
	if !strings.HasPrefix(e.Error.Message, "gang g1 of "+entry.ID+" did not form") || !strings.Contains(e.Error.Message, "n1") {
		t.Fatalf("the message must be the reason itself, naming the gang and the node: %q", e.Error.Message)
	}
}
