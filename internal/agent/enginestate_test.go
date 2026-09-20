package agent

import (
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/nodeapi"
)

// A pod can be Running while the engine inside it crash-loops, restarts on a
// bad flag, or never finishes loading. The worker is the only party that can
// say so — the leader sees a heartbeat, the platform sees a container.
func TestEngineStateSaysWhatTheProcessIsDoing(t *testing.T) {
	sup := NewSupervisor(nil)
	if engineState(sup) != nil {
		t.Fatal("no process: no statement, which is not the same as healthy")
	}
	if engineState(nil) != nil {
		t.Fatal("no supervisor (an RPC part, an engine the platform starts): no statement")
	}

	cases := []struct {
		status   string
		restarts int
		want     string
	}{
		{"running", 0, nodeapi.EngineServing},
		{"starting", 0, nodeapi.EngineStarting},
		{"crashloop", 7, nodeapi.EngineCrashLooping},
		{"failed", 1, nodeapi.EngineStopped},
		{"stopped", 0, nodeapi.EngineStopped},
		// Up right now, but it has been relaunched repeatedly: capacity that
		// keeps disappearing is not capacity.
		{"running", 3, nodeapi.EngineCrashLooping},
	}
	for _, c := range cases {
		sup := NewSupervisor(nil)
		sup.procs["engine"] = &Process{Info: ProcessInfo{ID: "engine", Status: c.status, Restarts: c.restarts, StartedAt: time.Now()}}
		got := engineState(sup)
		if got == nil || got.State != c.want {
			t.Errorf("%s (restarts %d) → %v, want %s", c.status, c.restarts, got, c.want)
		}
	}
}

// One dead engine beside healthy ones is what a reader needs to hear.
func TestTheWorstEngineSpeaksForTheWorker(t *testing.T) {
	sup := NewSupervisor(nil)
	sup.procs["a"] = &Process{Info: ProcessInfo{ID: "a", Status: "running", StartedAt: time.Now()}}
	sup.procs["b"] = &Process{Info: ProcessInfo{ID: "b", Status: "crashloop", Restarts: 4, ExitErr: "exit 1: CUDA error", StartedAt: time.Now()}}
	got := engineState(sup)
	if got == nil || got.State != nodeapi.EngineCrashLooping || got.Detail == "" {
		t.Fatalf("the worst one speaks, with its reason: %+v", got)
	}
}
