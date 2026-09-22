package agent

// T10.7: the worker loads the model it was started for ITSELF.
//
// The property under test is not "a model becomes resident" — the HTTP route
// already had that. It is that the load happens with NO bearer token: the
// container entrypoint used to POST this worker's own /v1/model/load with
// `Authorization: Bearer $OPOD_JOIN_TOKEN`, and a shell cannot sign HMAC, so
// RejectBearer (OPOD_REJECT_BEARER=1) refused the worker's own load and the
// switch could never be turned on.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/ollama" // the registry's native-name rule for "ollama"
)

// loadStub counts what a load did to the engine, and can fail the first n
// pulls — an engine that is still warming up.
type loadStub struct {
	engines.Engine
	mu        sync.Mutex
	name      string // "" = ollama, an engine that outlives its models
	healthErr error
	failPulls int
	pulls     []string
	loaded    []string
}

func (e *loadStub) Name() string {
	if e.name != "" {
		return e.name
	}
	return "ollama"
}
func (e *loadStub) Endpoint() string { return "http://127.0.0.1:1" }
func (e *loadStub) Health(context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.healthErr
}
func (e *loadStub) Pull(_ context.Context, name string, _ func(string, int64, int64)) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.pulls = append(e.pulls, name)
	if e.failPulls > 0 {
		e.failPulls--
		return errors.New("engine still warming")
	}
	return nil
}
func (e *loadStub) Load(_ context.Context, name string, _ bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.loaded = append(e.loaded, name)
	return nil
}
func (e *loadStub) List(context.Context) ([]string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.loaded, nil
}
func (e *loadStub) counts() (pulls, loaded int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.pulls), len(e.loaded)
}

func quickSelfLoad(t *testing.T) {
	t.Helper()
	wait, poll, retry := selfLoadWait, selfLoadPoll, selfLoadRetry
	selfLoadWait, selfLoadPoll, selfLoadRetry = 2*time.Second, 10*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { selfLoadWait, selfLoadPoll, selfLoadRetry = wait, poll, retry })
}

// The load a worker performs on itself does not authenticate at all, so the
// worker that refuses every bearer token still serves.
func TestTheWorkerLoadsItsOwnModelWithBearerRefused(t *testing.T) {
	quickSelfLoad(t)
	eng := &loadStub{}
	srv := &Server{Engine: eng, Token: "worker-token-for-test", RejectBearer: true, Aliases: &Aliases{}}
	srv.SelfLoad(context.Background(), LoadRequest{ID: "qwen2.5-0.5b-gguf", Repo: "org/repo", File: "m.gguf"})

	pulls, loaded := eng.counts()
	if pulls != 1 || loaded != 1 {
		t.Fatalf("the worker's own load did not reach the engine: %d pulls, %d loads", pulls, loaded)
	}
	// And the id is reported under the engine's native name, as the HTTP route
	// would have done — the heartbeat reads that table.
	if got := srv.Aliases.Native("qwen2.5-0.5b-gguf"); got == "" {
		t.Errorf("the alias the heartbeat reports was not noted: %q", got)
	}
}

// An engine that is still warming up is retried, and a load that never takes
// leaves the worker running: a registered worker with no model is a state the
// manager can act on, a dead container is not.
func TestSelfLoadRetriesThenLeavesTheWorkerAlive(t *testing.T) {
	quickSelfLoad(t)
	eng := &loadStub{failPulls: 2}
	srv := &Server{Engine: eng, Aliases: &Aliases{}}
	srv.SelfLoad(context.Background(), LoadRequest{ID: "m", Repo: "org/repo", File: "m.gguf"})
	if pulls, loaded := eng.counts(); pulls != 3 || loaded != 1 {
		t.Fatalf("want 3 pulls (two failures then a load), got %d pulls and %d loads", pulls, loaded)
	}

	always := &loadStub{failPulls: 99}
	srv = &Server{Engine: always, Aliases: &Aliases{}}
	done := make(chan struct{})
	go func() { srv.SelfLoad(context.Background(), LoadRequest{ID: "m"}); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("SelfLoad never gave up — a worker whose load fails must keep heartbeating")
	}
	if pulls, _ := always.counts(); pulls != selfLoadAttempts {
		t.Errorf("want %d attempts, got %d", selfLoadAttempts, pulls)
	}
}

// The worker waits for its engine before the first attempt (vLLM binds its
// port only after loading weights) and stops waiting when the process is
// asked to stop.
func TestSelfLoadWaitsForTheEngineAndHonoursCancellation(t *testing.T) {
	quickSelfLoad(t)
	eng := &loadStub{healthErr: errors.New("connection refused")}
	srv := &Server{Engine: eng, Aliases: &Aliases{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	srv.SelfLoad(ctx, LoadRequest{ID: "m"})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("a cancelled worker waited %v for its engine", elapsed)
	}
	if pulls, _ := eng.counts(); pulls != 0 {
		t.Errorf("a cancelled worker still tried to load: %d pulls", pulls)
	}
}

// An engine the load itself STARTS (vLLM, SGLang, llama.cpp) is never healthy
// before that load, so the worker must not wait for it: on the cell every
// llama.cpp worker sat through the full two-minute wait and then loaded fine.
// An engine that outlives its models (Ollama) IS waited for.
func TestAnEngineTheLoadStartsIsNotWaitedFor(t *testing.T) {
	quickSelfLoad(t)
	for _, c := range []struct {
		engine        string
		waitedAtLeast time.Duration
	}{
		{"llamacpp", 0},
		{"vllm", 0},
		{"ollama", selfLoadWait},
	} {
		eng := &loadStub{name: c.engine, healthErr: errors.New("connection refused")}
		// A launcher engine's load starts a process, so it needs the supervisor
		// `opod join` always gives the server.
		srv := &Server{Engine: eng, Aliases: &Aliases{}, Supervisor: NewSupervisor(nil)}
		start := time.Now()
		srv.SelfLoad(context.Background(), LoadRequest{ID: "m"})
		elapsed := time.Since(start)
		if c.waitedAtLeast == 0 && elapsed >= selfLoadWait {
			t.Errorf("%s: the load starts this engine, so the worker waited %v it did not have to", c.engine, elapsed)
		}
		if c.waitedAtLeast > 0 && elapsed < c.waitedAtLeast {
			t.Errorf("%s: an engine that outlives its models must be waited for, waited %v", c.engine, elapsed)
		}
		if pulls, _ := eng.counts(); pulls == 0 {
			t.Errorf("%s: the load never happened", c.engine)
		}
	}
}

// A model already whole in the node's cache is taken by path — no second
// download — unless the VERSION is pinned, where a file of the same NAME is
// not the bytes that were asked for.
func TestLoadBodyTakesTheCachedFileByPathUnlessTheVersionIsPinned(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "m.gguf"), []byte("weights"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := &Server{ModelsDir: dir}
	got := srv.LoadBody("m", "org/repo", "m.gguf")
	if got.Path != filepath.Join(dir, "m.gguf") || got.Repo != "" {
		t.Errorf("a cached file must be loaded by path, got %+v", got)
	}
	for _, pinned := range []*Server{
		{ModelsDir: dir, ModelRevision: "abc123"},
		{ModelsDir: dir, ModelSHA256: "deadbeef"},
	} {
		if got := pinned.LoadBody("m", "org/repo", "m.gguf"); got.Path != "" || got.Repo != "org/repo" {
			t.Errorf("a pinned version must be fetched, not taken by name: %+v", got)
		}
	}
	// Nothing cached, and a worker with no models directory at all (a door):
	// by repo and file.
	if got := (&Server{ModelsDir: dir}).LoadBody("m", "org/repo", "other.gguf"); got.Path != "" {
		t.Errorf("a file that is not there must not be loaded by path: %+v", got)
	}
	if got := (&Server{}).LoadBody("m", "org/repo", "m.gguf"); got.Repo != "org/repo" {
		t.Errorf("with no models dir the load goes by repo: %+v", got)
	}
}
