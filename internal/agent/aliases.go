package agent

// A worker's engine calls a model whatever its own loader calls it: llama-server
// names it by the Hugging Face repo, Ollama by its tag. The rest of the system
// — placements, routing, the control plane's plan — knows the model by the
// CATALOG ID it asked for. Something has to hold the two together.
//
// The leader used to do it by reverse-mapping the native name through its OWN
// bundled catalog. That works right up until the control plane knows a model
// the leader's image does not (a catalog entry added after the image was built,
// or a model the operator registered through the API): the reverse map misses,
// the placement is filed under the repo name, and a request for the model the
// operator created the endpoint for finds no worker — falling through to the
// leader's local engine, which answers something irrelevant like "engine vllm
// does not support embeddings". Found on the design-partner cell.
//
// So the worker remembers it instead. It was TOLD the id when it was told to
// load, which makes it the one place that never has to guess.

import "sync"

// Aliases maps an engine's native model name to the id it was loaded under.
// The zero value is usable and safe for concurrent use.
type Aliases struct {
	mu sync.RWMutex
	m  map[string]string
}

// Note records that native is this worker's copy of id. Loading the same
// native name under a new id replaces it: the last load wins, as it does in
// the engine.
func (a *Aliases) Note(native, id string) {
	if a == nil || native == "" || id == "" || native == id {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.m == nil {
		a.m = map[string]string{}
	}
	a.m[native] = id
}

// ID returns the id native was loaded under, or native itself when this worker
// was never told one (a model the engine had resident before we arrived).
func (a *Aliases) ID(native string) string {
	if a == nil {
		return native
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if id, ok := a.m[native]; ok {
		return id
	}
	return native
}

// Resolve maps a list of native names, dropping duplicates that collapse onto
// the same id (an engine can list the same weights under two spellings).
func (a *Aliases) Resolve(native []string) []string {
	out := make([]string, 0, len(native))
	seen := map[string]bool{}
	for _, n := range native {
		id := a.ID(n)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}
