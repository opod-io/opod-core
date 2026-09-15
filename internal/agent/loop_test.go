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
