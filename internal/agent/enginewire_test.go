package agent

import (
	"testing"
)

// The heartbeat's engine state is only ever as good as the supervisor the Agent
// was given — and for the life of the feature it was given none.
//
// `Agent.Procs` was declared and read (engineState(a.Procs)) but never assigned:
// cmd/opod/cmd_join.go built the Agent BEFORE it created the supervisor, then
// handed the supervisor to the worker's Server and not to the Agent. So the
// field stayed nil, engineState returned nil on every heartbeat, and "engine"
// was never sent — while the leader advertised feature "engine_liveness",
// ingested the field and counted zero unhealthy engines for ever.
//
// Found on the design-partner cell 2026-09-27: an engine crash-looped five times
// into `crashloop` and the endpoint read "converging" with an empty lastError.
//
// This test is about the CONTRACT that made that possible: a nil supervisor is
// indistinguishable from a healthy engine on the wire, so nothing downstream can
// tell "no statement" from "fine". Keep them distinguishable.
func TestANilSupervisorSaysNothingRatherThanHealthy(t *testing.T) {
	if st := engineState(nil); st != nil {
		t.Fatalf("a worker with no supervisor reported %+v — silence must not read as a state", st)
	}
}

// With a supervisor that has launched nothing, there is still no statement: an
// engine started by the platform (a llama.cpp RPC part) is not a dead engine.
func TestASupervisorWithNoProcessSaysNothing(t *testing.T) {
	sup := NewSupervisor(nil)
	if st := engineState(sup); st != nil {
		t.Fatalf("a supervisor that launched nothing reported %+v", st)
	}
}

// And a crash-looping process IS reported, with the engine's own last word —
// the half that reaches an operator instead of a pod log.
func TestACrashLoopingProcessIsReportedWithItsDetail(t *testing.T) {
	sup := NewSupervisor(nil)
	sup.mu.Lock()
	sup.procs["llama-server"] = &Process{Info: ProcessInfo{
		ID: "llama-server", Status: "crashloop", Restarts: 5,
		ExitErr: "exit status 1: invalid argument --nope"}}
	sup.mu.Unlock()

	st := engineState(sup)
	if st == nil {
		t.Fatal("a crash-looping engine reported nothing")
	}
	if st.State != "crash-looping" {
		t.Errorf("state = %q, want crash-looping", st.State)
	}
	if st.Restarts != 5 {
		t.Errorf("restarts = %d, want 5", st.Restarts)
	}
	if st.Detail == "" {
		t.Error("no detail — the operator is sent back to the pod log, which is the defect")
	}
}
