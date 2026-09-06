package engines

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Source is the catalog-side identity of a model as drivers need it: the
// catalog id plus the per-ecosystem names a driver may pull or serve by.
// Each driver decides which field is *its* native name (Descriptor.NativeName);
// the catalog and the leader never encode that knowledge themselves.
type Source struct {
	ID         string // catalog id, e.g. "llama-3-1-8b"
	OllamaName string // Ollama tag, e.g. "llama3.1:8b"
	Repo       string // Hugging Face repo, e.g. "meta-llama/Llama-3.1-8B-Instruct"
	Path       string // local weights path, e.g. a GGUF file
}

// Factory builds a driver for endpoint. apiKey is "" for engines without
// upstream auth; drivers that don't use it ignore it.
type Factory func(endpoint, apiKey string) Engine

// Descriptor is what a driver package registers from its init.
type Descriptor struct {
	// Name is the canonical engine name and must equal Engine.Name() of the
	// driver New returns (e.g. "vllm").
	Name string
	// Aliases are other spellings accepted in config, CLI flags and plans
	// (e.g. "tt-openai" for the vLLM/OpenAI driver, "llama-cpp" for llamacpp).
	Aliases []string
	// New constructs the driver. Required.
	New Factory
	// NativeName returns the identifier this engine pulls and serves a
	// catalog model by. Return "" to fall back to the catalog id.
	NativeName func(src Source) string
	// StartHint is one lowercase clause telling an operator how to start
	// the engine locally, e.g. "start it with: ollama serve".
	StartHint string
}

var (
	regMu     sync.RWMutex
	byName    = map[string]*Descriptor{} // canonical names and aliases
	canonical []string
)

// Register adds a driver. It is called from driver package inits; a
// duplicate name or alias is a programming error and panics at startup.
func Register(d Descriptor) {
	if d.Name == "" || d.New == nil {
		panic("engines.Register: Name and New are required")
	}
	names := make([]string, 0, 1+len(d.Aliases))
	for _, n := range append([]string{d.Name}, d.Aliases...) {
		names = append(names, strings.ToLower(strings.TrimSpace(n)))
	}
	regMu.Lock()
	defer regMu.Unlock()
	for _, n := range names {
		if prev, dup := byName[n]; dup {
			panic(fmt.Sprintf("engines.Register: %q already registered by %q", n, prev.Name))
		}
	}
	desc := d
	for _, n := range names {
		byName[n] = &desc
	}
	canonical = append(canonical, d.Name)
	sort.Strings(canonical)
}

// Lookup resolves a canonical name or alias (case-insensitive) to its
// Descriptor.
func Lookup(name string) (Descriptor, bool) {
	regMu.RLock()
	defer regMu.RUnlock()
	d, ok := byName[strings.ToLower(strings.TrimSpace(name))]
	if !ok {
		return Descriptor{}, false
	}
	return *d, true
}

// Canonical returns the canonical name for a name or alias, or the input
// unchanged when no driver claims it.
func Canonical(name string) string {
	if d, ok := Lookup(name); ok {
		return d.Name
	}
	return name
}

// Names lists the canonical names of every linked driver, sorted.
func Names() []string {
	regMu.RLock()
	defer regMu.RUnlock()
	return append([]string(nil), canonical...)
}

// New returns an Engine by name with no upstream auth. Convenience wrapper
// around NewWithAuth for backends that don't need an API key.
func New(name, endpoint string) (Engine, error) {
	return NewWithAuth(name, endpoint, "")
}

// NewWithAuth returns an Engine by canonical name or alias. The apiKey is
// forwarded to drivers whose upstream supports Bearer auth (vLLM / tt-openai)
// and ignored by the rest. Unknown names list what IS linked, which is the
// usual symptom of a build that forgot to import internal/engines/all.
func NewWithAuth(name, endpoint, apiKey string) (Engine, error) {
	d, ok := Lookup(name)
	if !ok {
		return nil, fmt.Errorf("unknown engine %q (linked drivers: %s)", name, strings.Join(Names(), ", "))
	}
	return d.New(endpoint, apiKey), nil
}

// MustNew is NewWithAuth for call sites whose engine name is a constant
// (the router's remote-worker and shard-coordinator clients). It can only
// fail when the driver package isn't linked into the binary, so it panics
// with that diagnosis rather than returning an error every caller would
// have to treat as impossible.
func MustNew(name, endpoint, apiKey string) Engine {
	eng, err := NewWithAuth(name, endpoint, apiKey)
	if err != nil {
		panic("engines: " + err.Error() + " — import internal/engines/all (or the driver package) in the binary")
	}
	return eng
}

// StartHint returns the driver's operator hint for starting the engine, or
// a generic line for unknown engines.
func StartHint(name string) string {
	if d, ok := Lookup(name); ok && d.StartHint != "" {
		return d.StartHint
	}
	return "start the configured engine, then check `opod status`"
}

// NativeName maps a catalog source to the name the named engine pulls and
// serves by, falling back to the catalog id. Both the worker agent and the
// catalog layer resolve names through here — each side passing its OWN
// engine name, which is what makes heterogeneous fleets correct.
func NativeName(engine string, src Source) string {
	if d, ok := Lookup(engine); ok && d.NativeName != nil {
		if n := d.NativeName(src); n != "" {
			return n
		}
	}
	return src.ID
}

// CatalogID is the inverse of NativeName: it maps a name an engine reports
// (its native name for the model, or the catalog id itself — vLLM lists
// both) back to the catalog id, so placements key on the id the router
// looks up. engine selects the driver whose naming applies; "" (the leader
// does not know which engine a worker runs) consults every linked driver.
// As a last resort any field of a Source matches, so an unlinked or
// unknown driver never produces a phantom model. Returns native unchanged
// when nothing matches (custom/scheme ids).
func CatalogID(engine, native string, sources []Source) string {
	for _, src := range sources {
		if native == src.ID {
			return src.ID
		}
	}
	for _, d := range candidates(engine) {
		if d.NativeName == nil {
			continue
		}
		for _, src := range sources {
			if d.NativeName(src) == native {
				return src.ID
			}
		}
	}
	for _, src := range sources {
		if native == src.OllamaName || native == src.Repo || native == src.Path {
			return src.ID
		}
	}
	return native
}

func candidates(engine string) []Descriptor {
	if engine != "" {
		if d, ok := Lookup(engine); ok {
			return []Descriptor{d}
		}
		return nil
	}
	regMu.RLock()
	defer regMu.RUnlock()
	out := make([]Descriptor, 0, len(canonical))
	for _, n := range canonical {
		out = append(out, *byName[n])
	}
	return out
}
