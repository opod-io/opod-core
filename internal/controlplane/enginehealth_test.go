package controlplane

import (
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/nodeapi"
)

// /loadz says how many workers a leader has. This is how much of that count is
// actually serving: a worker whose engine crash-loops heartbeats like any
// other and holds its card, so a scaler that believes the count scales out too
// late or not at all.
func TestUnhealthyEnginesAreCounted(t *testing.T) {
	s := &Server{}
	store := func(id, state, detail string, age time.Duration) {
		s.nodeEngine.Store(id, nodeEngineSample{
			EngineState: nodeapi.EngineState{State: state, Detail: detail},
			at:          time.Now().Add(-age)})
	}
	store("n1", nodeapi.EngineServing, "", 0)
	store("n2", nodeapi.EngineCrashLooping, "exit 1: CUDA error", 0)
	store("n3", nodeapi.EngineStopped, "no such file", 0)
	store("n4", nodeapi.EngineStarting, "", 0) // loading a large model is not a fault
	// Stale: the worker stopped saying anything, which liveness handles — this
	// must not be counted twice or reported as a dead engine.
	store("n5", nodeapi.EngineCrashLooping, "old", 5*time.Minute)

	n, worst := s.EnginesUnhealthy()
	if n != 2 {
		t.Fatalf("unhealthy %d, want 2 (crash-looping + stopped; starting and stale are not)", n)
	}
	if worst == "" || worst[:len(nodeapi.EngineCrashLooping)] != nodeapi.EngineCrashLooping {
		t.Errorf("the worst one speaks: %q", worst)
	}
}
