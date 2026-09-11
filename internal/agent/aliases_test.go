package agent

import (
	"reflect"
	"testing"
)

// The leader files placements under whatever the heartbeat reports. Reporting
// the engine's native name (a Hugging Face repo) instead of the id the model
// was loaded under made an endpoint unroutable whenever the leader's own
// bundled catalog did not contain that model: the request fell through to the
// leader's local engine and failed for an unrelated reason.
func TestAliasesReportTheLoadedIdentity(t *testing.T) {
	var a *Aliases // a worker that was never told anything still works
	if got := a.ID("nomic-ai/nomic-embed-text-v1.5-GGUF"); got != "nomic-ai/nomic-embed-text-v1.5-GGUF" {
		t.Errorf("nil Aliases = %q, want the native name unchanged", got)
	}
	if got := a.Resolve([]string{"x", "y"}); !reflect.DeepEqual(got, []string{"x", "y"}) {
		t.Errorf("nil Resolve = %v", got)
	}

	al := &Aliases{}
	al.Note("nomic-ai/nomic-embed-text-v1.5-GGUF", "nomic-embed-text-v1.5-gguf")
	al.Note("bartowski/Llama-3.2-3B-Instruct-GGUF", "llama-3.2-3b-gguf")
	al.Note("llama3.2:1b", "llama3.2:1b") // native == id: nothing to remember
	if got := al.ID("nomic-ai/nomic-embed-text-v1.5-GGUF"); got != "nomic-embed-text-v1.5-gguf" {
		t.Errorf("ID = %q", got)
	}
	// A model the engine had resident before we arrived keeps its own name
	// rather than disappearing from the heartbeat.
	if got := al.ID("someone/else"); got != "someone/else" {
		t.Errorf("unknown native = %q", got)
	}

	want := []string{"nomic-embed-text-v1.5-gguf", "llama-3.2-3b-gguf", "someone/else"}
	got := al.Resolve([]string{"nomic-ai/nomic-embed-text-v1.5-GGUF", "bartowski/Llama-3.2-3B-Instruct-GGUF", "someone/else"})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Resolve = %v, want %v", got, want)
	}

	// Two spellings of the same weights collapse to one placement row.
	al.Note("org/Repo-GGUF", "m")
	al.Note("org/repo-gguf", "m")
	if got := al.Resolve([]string{"org/Repo-GGUF", "org/repo-gguf"}); !reflect.DeepEqual(got, []string{"m"}) {
		t.Errorf("duplicate ids = %v, want one row", got)
	}

	// A re-load under a new id wins, as it does in the engine.
	al.Note("org/Repo-GGUF", "m2")
	if got := al.ID("org/Repo-GGUF"); got != "m2" {
		t.Errorf("re-load = %q, want m2", got)
	}
}
