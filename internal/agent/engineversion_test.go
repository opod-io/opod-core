package agent

import (
	"context"
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/pkg/nodeapi"
)

type versionStub struct {
	engines.Engine
	asked int
}

func (v *versionStub) Version(context.Context) (string, error) { v.asked++; return "0.33.0", nil }

// PLAN T29.5: the engine's version rides the heartbeat's engine state. It is
// asked only once the engine serves (a starting engine has nothing to answer
// with) and kept: the image fixes the binary for the worker's life.
func TestTheEngineVersionIsReadOnceItServes(t *testing.T) {
	eng := &versionStub{}
	a := &Agent{Engine: eng}
	if v := a.engineVersionOf(context.Background(), &nodeapi.EngineState{State: nodeapi.EngineStarting}); v != "" || eng.asked != 0 {
		t.Fatalf("a starting engine is not asked: %q, asked %d", v, eng.asked)
	}
	for i := 0; i < 3; i++ {
		if v := a.engineVersionOf(context.Background(), &nodeapi.EngineState{State: nodeapi.EngineServing}); v != "0.33.0" {
			t.Fatalf("serving: %q", v)
		}
	}
	if eng.asked != 1 {
		t.Fatalf("asked %d times, want once", eng.asked)
	}
	// An engine that does not state a version: nothing, and no error.
	if v := (&Agent{Engine: &listStub{}}).engineVersionOf(context.Background(), &nodeapi.EngineState{State: nodeapi.EngineServing}); v != "" {
		t.Fatalf("no Versioner: %q", v)
	}
}
