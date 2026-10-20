package agent

// R10.1 / R10.2: the agent carries one boot id per process on register and
// every heartbeat, and exits only when the leader refuses its re-register
// MaxRefusedRegisters times in a row — never over one 401.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeLeader struct {
	srv         *httptest.Server
	mu          sync.Mutex
	registerRC  int
	heartbeatRC int
	registers   int
	bootIDs     map[string]bool
}

func newFakeLeader(t *testing.T) *fakeLeader {
	t.Helper()
	f := &fakeLeader{registerRC: 200, heartbeatRC: 200, bootIDs: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		if id, _ := body["boot_id"].(string); id != "" {
			f.bootIDs[id] = true
		}
		if strings.HasSuffix(r.URL.Path, "/register") {
			f.registers++
			w.WriteHeader(f.registerRC)
			return
		}
		w.WriteHeader(f.heartbeatRC)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLeader) set(register, heartbeat int) {
	f.mu.Lock()
	f.registerRC, f.heartbeatRC = register, heartbeat
	f.mu.Unlock()
}

func newAgent(f *fakeLeader) *Agent {
	return &Agent{NodeID: "n1", LeaderURL: f.srv.URL, Token: "sk", HeartbeatInterval: 2 * time.Millisecond,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestLoopCarriesOneBootIDPerProcess(t *testing.T) {
	f := newFakeLeader(t)
	a := newAgent(f)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := a.Loop(ctx); err != context.DeadlineExceeded {
		t.Fatalf("a healthy loop runs until its context ends: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.BootID == "" || len(f.bootIDs) != 1 || !f.bootIDs[a.BootID] {
		t.Fatalf("register and every heartbeat carry the one boot id %q: %v", a.BootID, f.bootIDs)
	}
	first, second := NewBootID(), NewBootID()
	if first == second {
		t.Fatal("boot ids are unique per process")
	}
}

func TestLoopSurvivesAHeartbeat401WhileRegisterIsAccepted(t *testing.T) {
	f := newFakeLeader(t)
	f.set(200, 401) // the leader restarted: heartbeats refused, re-register accepted
	a := newAgent(f)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if err := a.Loop(ctx); err != context.DeadlineExceeded {
		t.Fatalf("a 401 storm on heartbeats with a working re-register never exits: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.registers < MaxRefusedRegisters+2 {
		t.Fatalf("re-registered on every 401 (%d registers)", f.registers)
	}
}

func TestLoopExitsWhenTheRegisterItselfIsRefusedRepeatedly(t *testing.T) {
	f := newFakeLeader(t)
	f.set(401, 401) // the token is dead: the endpoint that minted it is gone
	a := newAgent(f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := a.Loop(ctx)
	if err == nil || err == context.DeadlineExceeded || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("a refused re-register %d times running exits with a clear error: %v", MaxRefusedRegisters, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// The initial register + MaxRefusedRegisters refused re-registers.
	if f.registers != MaxRefusedRegisters+1 {
		t.Fatalf("exactly %d refused re-registers before exiting, got %d registers", MaxRefusedRegisters, f.registers)
	}
}

// An engine that serves again is announced at once (PLAN T8.22): the leader
// refuses the model until a heartbeat says it is loaded, and the timed one is
// up to an interval away — on the design-partner cell every request between a
// relaunch and the next beat (≈5 s) was refused. A change on the supervisor
// fires the beat early; with an hour-long interval, only that can beat.
func TestAnEngineChangeSendsTheHeartbeatAtOnce(t *testing.T) {
	var mu sync.Mutex
	beats := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			mu.Lock()
			beats++
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	a := &Agent{NodeID: "n1", LeaderURL: srv.URL, Token: "sk", HeartbeatInterval: time.Hour, Procs: sup,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Loop(ctx) }()
	count := func() int { mu.Lock(); defer mu.Unlock(); return beats }
	time.Sleep(50 * time.Millisecond)
	if n := count(); n != 0 {
		t.Fatalf("no change yet, hour-long interval: %d heartbeats", n)
	}
	sup.notifyChanged()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && count() == 0; time.Sleep(5 * time.Millisecond) {
	}
	time.Sleep(50 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("a change sends one heartbeat at once, got %d", n)
	}
}

// Changes that arrive while one is pending are covered by it: two with no
// reader leave one signal, so a burst of supervisor changes is one early beat.
// Checked on the supervisor alone — with the loop running, whether the second
// change lands before the loop takes the first is the scheduler's choice.
func TestSupervisorChangesCoalesce(t *testing.T) {
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	sup.notifyChanged()
	sup.notifyChanged()
	if n := len(sup.changed); n != 1 {
		t.Fatalf("two changes with no reader leave one signal, got %d", n)
	}
}

// "Running" is not "loaded": an engine starts with no readiness probe, so a
// heartbeat on the process coming up told the leader the engine served
// nothing — on the design-partner cell the 503 then read "waking" for the
// whole load. The loop asks the engine until it lists a model, and beats then.
func TestARelaunchedEngineIsAnnouncedWhenItListsAModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("script-based test relies on POSIX shell")
	}
	var mu sync.Mutex
	beats, listed := 0, false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			mu.Lock()
			beats++
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	eng := &listStub{list: func(context.Context) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		if listed {
			return []string{"m"}, nil
		}
		return []string{}, nil
	}}
	sup := NewSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer sup.StopAll()
	a := &Agent{NodeID: "n1", LeaderURL: srv.URL, Token: "sk", HeartbeatInterval: time.Hour, Procs: sup, Engine: eng,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = a.Loop(ctx) }()
	time.Sleep(50 * time.Millisecond)
	if _, err := sup.Start(context.Background(), ProcessSpec{ID: "engine", Command: "/bin/sh", Args: []string{"-c", "sleep 30"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return beats }
	time.Sleep(3 * engineWatchPoll)
	if n := count(); n != 0 {
		t.Fatalf("up but listing nothing: no early heartbeat, got %d", n)
	}
	mu.Lock()
	listed = true
	mu.Unlock()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline) && count() == 0; time.Sleep(20 * time.Millisecond) {
	}
	if n := count(); n != 1 {
		t.Fatalf("the engine lists a model: one heartbeat at once, got %d", n)
	}
}
