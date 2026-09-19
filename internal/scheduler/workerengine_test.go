package scheduler

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/agent"
	_ "github.com/opod-io/opod/internal/engines/all" // the real drivers' SingleModel facts
	"github.com/opod-io/opod/internal/store"
)

func nodeRunning(t *testing.T, id, engine string) store.Node {
	t.Helper()
	hw, err := json.Marshal(agent.Capabilities{Hostname: id, OS: "linux", Arch: "amd64", Engine: engine})
	if err != nil {
		t.Fatal(err)
	}
	return store.Node{ID: id, HardwareJSON: string(hw)}
}

// The leader refuses a load that would silently stop another model: on a
// one-model-per-process engine, and — the blunt rule — on a worker whose
// engine it does not know. It names both models. An engine that holds several
// models, a worker that serves nothing else, and the model's own adapters are
// no refusal.
func TestLoadWouldReplace(t *testing.T) {
	serving := func(ids ...string) []store.Placement {
		out := make([]store.Placement, 0, len(ids))
		for _, id := range ids {
			out = append(out, store.Placement{ModelID: id, Status: "ready"})
		}
		return out
	}
	for _, engine := range []string{"vllm", "sglang", "llamacpp", "llama-cpp", "tt"} {
		err := LoadWouldReplace(nodeRunning(t, "w1", engine), serving("qwen3-8b"), "llama-3-8b")
		if !errors.Is(err, ErrUnplaceable) || !strings.Contains(err.Error(), "qwen3-8b") || !strings.Contains(err.Error(), "llama-3-8b") ||
			!strings.Contains(err.Error(), "one model per process") {
			t.Errorf("%s serving another model: %v", engine, err)
		}
		if err := LoadWouldReplace(nodeRunning(t, "w1", engine), nil, "llama-3-8b"); err != nil {
			t.Errorf("%s serving nothing: %v", engine, err)
		}
		if err := LoadWouldReplace(nodeRunning(t, "w1", engine), serving("llama-3-8b", "llama-3-8b:support"), "llama-3-8b"); err != nil {
			t.Errorf("%s serving the model itself and its adapter: %v", engine, err)
		}
	}
	for _, engine := range []string{"ollama", "mlx"} {
		if err := LoadWouldReplace(nodeRunning(t, "w1", engine), serving("qwen3-8b"), "llama-3-8b"); err != nil {
			t.Errorf("%s holds several models, a load stops nothing: %v", engine, err)
		}
	}
	err := LoadWouldReplace(store.Node{ID: "old"}, serving("qwen3-8b"), "llama-3-8b")
	if !errors.Is(err, ErrUnplaceable) || !strings.Contains(err.Error(), "did not register") || !strings.Contains(err.Error(), "qwen3-8b") {
		t.Errorf("an unregistered engine gets the blunt rule: %v", err)
	}
	if err := LoadWouldReplace(store.Node{ID: "old"}, nil, "llama-3-8b"); err != nil {
		t.Errorf("an unregistered engine serving nothing: %v", err)
	}
	err = LoadWouldReplace(nodeRunning(t, "w1", "somethingelse"), serving("qwen3-8b"), "llama-3-8b")
	if !errors.Is(err, ErrUnplaceable) || !strings.Contains(err.Error(), "somethingelse") {
		t.Errorf("an engine this binary does not link gets the blunt rule: %v", err)
	}
	if got := WorkerEngine(nodeRunning(t, "w1", "vllm")); got != "vllm" {
		t.Errorf("WorkerEngine = %q", got)
	}
}
