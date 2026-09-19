package scheduler

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// WorkerEngine is the engine id a worker registered with its hardware
// (agent.Capabilities.Engine), "" for a worker that predates the field.
func WorkerEngine(n store.Node) string {
	var caps agent.Capabilities
	if n.HardwareJSON == "" || json.Unmarshal([]byte(n.HardwareJSON), &caps) != nil {
		return ""
	}
	return caps.Engine
}

// LoadWouldReplace answers "what would /v1/model/load of model do to what
// this worker already serves?" — nil when nothing is lost, else a refusal
// that names both models.
//
// A worker whose engine serves one model per process (engines.SingleModel:
// vLLM, SGLang, llama.cpp) stops what it serves to start the new model, so a
// load there is a replacement, not an addition. An engine that holds several
// (Ollama, MLX) loses nothing. A worker that did not register its engine, or
// registered one this binary does not link, is judged by the only safe rule:
// if it serves anything else, refuse. placed is the node's placement rows;
// the model itself, its own adapter variants ("<model>:<adapter>") and a row
// the leader released (store.PlacementReleased) are not "something else".
func LoadWouldReplace(n store.Node, placed []store.Placement, model string) error {
	var others []string
	for _, p := range placed {
		if p.ModelID == model || strings.HasPrefix(p.ModelID, model+":") || p.Status == store.PlacementReleased {
			continue // released: installed there, deliberately not served
		}
		others = append(others, p.ModelID)
	}
	if len(others) == 0 {
		return nil
	}
	sort.Strings(others)
	serving := strings.Join(others, ", ")
	engine := WorkerEngine(n)
	single, known := engines.SingleModel(engine)
	switch {
	case known && !single:
		return nil
	case known:
		return fmt.Errorf("%w: node %s runs %s, which serves one model per process, and already serves %s — loading %s there would stop it; unload %s from %s first, or pick another node",
			ErrUnplaceable, n.ID, engine, serving, model, serving, n.ID)
	case engine == "":
		return fmt.Errorf("%w: node %s serves %s and did not register which engine it runs (a worker from before that field), so the leader cannot tell whether loading %s there would stop it; pick a node that serves nothing else, or restart the worker on a current version",
			ErrUnplaceable, n.ID, serving, model)
	default:
		return fmt.Errorf("%w: node %s serves %s and runs engine %q, which this leader does not know, so it cannot tell whether loading %s there would stop it; pick a node that serves nothing else",
			ErrUnplaceable, n.ID, serving, engine, model)
	}
}
