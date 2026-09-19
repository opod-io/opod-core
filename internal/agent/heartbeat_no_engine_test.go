package agent

import (
	"context"
	"errors"
	"syscall"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

// listStub answers List the way a scripted engine would.
type listStub struct {
	engines.Engine
	list func(ctx context.Context) ([]string, error)
}

func (e *listStub) Name() string                               { return "llamacpp" }
func (e *listStub) List(ctx context.Context) ([]string, error) { return e.list(ctx) }

// "No report" (loaded_models: null) means the engine did not answer IN TIME —
// the leader then changes nothing and, past its patience, takes the worker out
// of rotation as engine-silent. An engine that is NOT RUNNING is a different
// fact, and a definite one: nothing listens, so nothing is loaded — `[]`.
// Treating it as "no report" took every worker whose engine is launched by a
// load (vLLM, SGLang, llama.cpp) out of rotation 30 s after it joined, and
// every part of a llama.cpp RPC gang for ever: a part runs an rpc-server, never
// an engine, so its leader never became ready (cluster, 2026-09-19).
func TestAnEngineThatIsNotRunningReportsNothingLoaded(t *testing.T) {
	heartbeat := func(a *Agent) error { _, err := a.Heartbeat(context.Background()); return err }
	refused := &listStub{list: func(context.Context) ([]string, error) {
		return nil, engines.Unreachable("llamacpp", "http://127.0.0.1:8089", syscall.ECONNREFUSED)
	}}
	if body := capturedPost(t, heartbeat, refused, &Aliases{}, Capabilities{}); string(body["loaded_models"]) != "[]" {
		t.Fatalf("nothing is listening: want loaded_models [] (nothing loaded), got %s", body["loaded_models"])
	}

	slow := &listStub{list: func(ctx context.Context) ([]string, error) {
		<-ctx.Done() // a hung engine: the heartbeat's own 2 s patience runs out
		return nil, engines.Unreachable("llamacpp", "http://127.0.0.1:8089", ctx.Err())
	}}
	start := time.Now()
	if body := capturedPost(t, heartbeat, slow, &Aliases{}, Capabilities{}); string(body["loaded_models"]) != "null" {
		t.Fatalf("the engine did not answer in time: want loaded_models null (no report), got %s", body["loaded_models"])
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("a hung engine must not hold the heartbeat")
	}

	broken := &listStub{list: func(context.Context) ([]string, error) { return nil, errors.New("500 from the engine") }}
	if body := capturedPost(t, heartbeat, broken, &Aliases{}, Capabilities{}); string(body["loaded_models"]) != "null" {
		t.Fatalf("an engine that answers with an error says nothing about what it holds: want null, got %s", body["loaded_models"])
	}
}
