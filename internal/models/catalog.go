// Package models loads catalog entries (YAML files describing known models)
// and implements model-selection helpers.
package models

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opod-io/opod/internal/engines"
	"gopkg.in/yaml.v3"
)

// Entry is a single catalog entry, loaded from catalog/<id>.yaml.
type Entry struct {
	ID                 string       `yaml:"id"                            json:"id"`
	DisplayName        string       `yaml:"display_name"                  json:"display_name"`
	Source             SourceSpec   `yaml:"source"                        json:"source"`
	SizeBytes          int64        `yaml:"size_bytes"                    json:"size_bytes"`
	Quant              string       `yaml:"quant"                         json:"quant"`
	ContextWindow      int          `yaml:"context_window"                json:"context_window"`
	Capabilities       []string     `yaml:"capabilities"                  json:"capabilities"`
	RecommendedEngines []string     `yaml:"recommended_engines"           json:"recommended_engines"`
	Hardware           HardwareSpec `yaml:"hardware"                      json:"hardware"`
	Tags               []string     `yaml:"tags"                          json:"tags"`
	Sharding           ShardingSpec `yaml:"sharding,omitempty"            json:"sharding,omitempty"`

	// License is a short identifier (SPDX where possible) of the model's
	// release license. Examples: "apache-2.0", "mit", "llama-3-community",
	// "llama-4-community", "gemma", "minisign-restricted". Surfaced in
	// `opod model info` so commercial users see it before install.
	License string `yaml:"license,omitempty" json:"license,omitempty"`
	// LicenseURL points at the canonical license text — usually the
	// model's HuggingFace LICENSE file.
	LicenseURL string `yaml:"license_url,omitempty" json:"license_url,omitempty"`

	// Released is the model's public release date in YYYY-MM-DD form.
	// Lets users sort/filter the catalog by recency (`opod model search`
	// shows it; `opod model info` renders the full date). Approximate
	// is fine — use the month if a precise day isn't known, e.g.
	// "2024-09-01" rather than guessing.
	Released string `yaml:"released,omitempty" json:"released,omitempty"`

	// Fallback is the GENERIC ordered fallback chain — tried when the
	// router can't classify the primary's failure into a more specific
	// category. Engine down, model not loaded, generic 5xx, timeout.
	// Silent to clients — the response carries the requested model name.
	// Operators see fallback hits in the audit log + stderr.
	Fallback []string `yaml:"fallback,omitempty" json:"fallback,omitempty"`

	// FallbackOnContextLength replaces the generic chain when the primary
	// rejects with a context-length-exceeded error. Typically points at
	// long-context variants of the same family (e.g. an `n_ctx=128k`
	// llama-server config or a Yi/Qwen long-context build). Empty falls
	// back to `Fallback`.
	FallbackOnContextLength []string `yaml:"fallback_on_context_length,omitempty" json:"fallback_on_context_length,omitempty"`

	// FallbackOnContentPolicy replaces the generic chain when the
	// upstream (usually a vendor — Anthropic, OpenAI) refuses on content
	// policy grounds. Typically points at an open-weight or
	// permissively-aligned model. Empty falls back to `Fallback`.
	FallbackOnContentPolicy []string `yaml:"fallback_on_content_policy,omitempty" json:"fallback_on_content_policy,omitempty"`

	// PricePromptUSDPer1K and PriceCompletionUSDPer1K are the pricing
	// rates per 1k tokens. 0 (default) means "free / no cost tracking" —
	// the right answer for open-weight models running on the operator's
	// hardware. Non-zero values are typically used by vendor proxies
	// (Claude, GPT, etc., but the vendor table in `vendor_pricing.go`
	// covers those automatically) or by operators who want to internally
	// charge departments for compute time.
	PricePromptUSDPer1K     float64 `yaml:"price_prompt_usd_per_1k,omitempty" json:"price_prompt_usd_per_1k,omitempty"`
	PriceCompletionUSDPer1K float64 `yaml:"price_completion_usd_per_1k,omitempty" json:"price_completion_usd_per_1k,omitempty"`
}

// SourceSpec describes where to fetch model weights from.
type SourceSpec struct {
	Type       string `yaml:"type"                    json:"type"` // ollama | huggingface | file
	Repo       string `yaml:"repo,omitempty"          json:"repo,omitempty"`
	File       string `yaml:"file,omitempty"          json:"file,omitempty"` // specific file within an HF repo (for GGUF)
	OllamaName string `yaml:"ollama_name,omitempty"   json:"ollama_name,omitempty"`
	Path       string `yaml:"path,omitempty"          json:"path,omitempty"` // local filesystem path (for GGUF / safetensors)
}

// ShardingSpec is set when a model is too large for any single node and must
// be split across several. When Required is true, the model can only be
// served via the auto-orchestrator: rpc-server on each shard host + a
// coordinator running `llama-server --rpc <list>`. A shard count of 1 is the
// no-split degenerate case: no rpc-servers, one whole llama-server on the
// single chosen host — so "required: true" entries can still be served on
// one machine when it has the RAM.
type ShardingSpec struct {
	Required        bool   `yaml:"required"          json:"required"`
	DefaultShards   int    `yaml:"default_shards"    json:"default_shards"`
	Engine          string `yaml:"engine"            json:"engine"`           // "llamacpp" (only supported in v0.4)
	RPCPortBase     int    `yaml:"rpc_port_base"     json:"rpc_port_base"`    // workers bind rpc-server to this + shard index
	CoordinatorPort int    `yaml:"coordinator_port"  json:"coordinator_port"` // coordinator binds llama-server to this
}

// HardwareSpec describes the minimum hardware a model needs to run reasonably.
type HardwareSpec struct {
	MinRAMGB  int `yaml:"min_ram_gb"             json:"min_ram_gb"`
	MinVRAMGB int `yaml:"min_vram_gb,omitempty"  json:"min_vram_gb,omitempty"`
}

// LoadCatalog reads every *.yaml file in dir (non-recursive) and returns parsed entries.
// If dir is empty, the built-in resolution order is walked and **all**
// directories that exist are merged. Later directories override earlier
// ones on ID collision — so a user-supplied entry in
// `~/.opod/catalog/` wins over the binary's bundled
// `./catalog/<same-id>.yaml`. The precedence makes "edit-locally to
// override" actually work; before the merge change the user's drop-in
// was silently shadowed.
//
// Merge precedence (last writer wins):
//
//  1. $OPOD_CATALOG_DIR        (least authoritative)
//  2. ./catalog (relative to cwd)
//  3. <exe-dir>/catalog
//  4. /usr/local/share/opod/catalog
//  5. /usr/share/opod/catalog
//  6. ~/.opod/catalog          (most authoritative — user overrides)
//
// An explicit non-empty dir argument skips the merge and reads only
// that directory (used by tests and callers that know exactly what they
// want).
//
// Entries are returned sorted by SizeBytes ascending on every path —
// AutoPick (and anything else scanning for "largest that fits") relies
// on that invariant.
func LoadCatalog(dir string) ([]Entry, error) {
	var out []Entry
	if dir != "" {
		got, err := readCatalogDir(dir)
		if err != nil {
			return nil, err
		}
		out = got
	} else {
		dirs := resolveCatalogDirs()
		if len(dirs) == 0 {
			return nil, fmt.Errorf("no catalog directory found")
		}
		// Merge into a map keyed by ID; later directories overwrite earlier
		// ones. We do not warn the user about a shadowed entry — there's no
		// good way to surface it without spamming startup logs. The
		// dashboard's Catalog browser shows the *effective* entry, which is
		// what matters operationally.
		merged := map[string]Entry{}
		for _, d := range dirs {
			got, err := readCatalogDir(d)
			if err != nil {
				return nil, err
			}
			for _, e := range got {
				merged[e.ID] = e
			}
		}
		out = make([]Entry, 0, len(merged))
		for _, e := range merged {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SizeBytes < out[j].SizeBytes })
	return out, nil
}

// readCatalogDir reads a single directory's worth of YAML files into
// Entry values. Used both by the explicit-dir path and by the merge.
func readCatalogDir(dir string) ([]Entry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", dir, err)
	}
	var out []Entry
	for _, de := range entries {
		if de.IsDir() {
			continue
		}
		if !strings.HasSuffix(de.Name(), ".yaml") && !strings.HasSuffix(de.Name(), ".yml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, de.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", de.Name(), err)
		}
		var e Entry
		if err := yaml.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("parse %s: %w", de.Name(), err)
		}
		if e.ID == "" {
			return nil, fmt.Errorf("%s: missing id", de.Name())
		}
		out = append(out, e)
	}
	return out, nil
}

// FindByID returns the entry with the given id, or nil.
func FindByID(entries []Entry, id string) *Entry {
	for i := range entries {
		if entries[i].ID == id {
			return &entries[i]
		}
	}
	return nil
}

// EngineNativeName maps a catalog Entry to the model name a given engine pulls
// by: the Ollama tag for ollama, the HF repo (or local GGUF path) for
// vllm/mlx/tt/llamacpp. Falls back to the entry ID when the source has no name
// for that engine. Shared so the CLI (`opod model add`, against the leader's
// engine) and the worker agent (`/v1/model/load`, against its own engine)
// resolve names identically — heterogeneous fleets resolve per their local
// engine, which is exactly why the worker must compute this itself.
func EngineNativeName(engine string, e *Entry) string {
	return engines.NativeName(engine, engines.Source{
		ID: e.ID, OllamaName: e.Source.OllamaName, Repo: e.Source.Repo, Path: e.Source.Path,
	})
}

// ParseSchemeID recognizes the `hf:`, `ollama:`, and `file:` prefixes used by
// `opod model add` to pull a model that has no curated catalog entry, and
// returns a synthetic Entry the install flow can consume.
//
//   - hf:owner/repo            → Source.Type=huggingface, Repo=owner/repo
//   - hf:owner/repo:file.gguf  → Source.Type=huggingface, Repo=owner/repo, File=file.gguf
//   - ollama:phi3              → Source.Type=ollama, OllamaName=phi3
//   - ollama:phi3:mini         → Source.Type=ollama, OllamaName=phi3:mini
//   - file:/abs/path/x.gguf    → Source.Type=file, Path=/abs/path/x.gguf
//
// The synthetic entry uses the full scheme-prefixed id as both ID and
// DisplayName, and leaves hardware/size unset so the install flow knows to
// skip the hardware-floor check. Returns (nil, false) for ids that don't
// match any known scheme — callers should fall through to catalog lookup.
func ParseSchemeID(id string) (*Entry, bool) {
	switch {
	case strings.HasPrefix(id, "hf:"):
		rest := strings.TrimPrefix(id, "hf:")
		if rest == "" || !strings.Contains(rest, "/") {
			return nil, false
		}
		repo, file := rest, ""
		// Optional "owner/repo:filename.gguf" — split on the last colon
		// after the slash so colons inside the repo path (none today, but
		// future-proof) wouldn't break parsing.
		if i := strings.LastIndex(rest, ":"); i > strings.Index(rest, "/") {
			repo, file = rest[:i], rest[i+1:]
		}
		return &Entry{
			ID:          id,
			DisplayName: id,
			Source:      SourceSpec{Type: "huggingface", Repo: repo, File: file},
		}, true
	case strings.HasPrefix(id, "ollama:"):
		name := strings.TrimPrefix(id, "ollama:")
		if name == "" {
			return nil, false
		}
		return &Entry{
			ID:          id,
			DisplayName: id,
			Source:      SourceSpec{Type: "ollama", OllamaName: name},
		}, true
	case strings.HasPrefix(id, "file:"):
		path := strings.TrimPrefix(id, "file:")
		if path == "" {
			return nil, false
		}
		return &Entry{
			ID:          id,
			DisplayName: id,
			Source:      SourceSpec{Type: "file", Path: path},
		}, true
	}
	return nil, false
}

// resolveCatalogDirs returns every catalog directory that exists, in
// merge order: least-authoritative first, most-authoritative last. The
// home-config dir (`~/.opod/catalog`) is placed last so user-edited
// entries override the binary's bundled catalog on ID collision.
//
// Earlier this returned a single directory and stopped at the first
// match, which silently shadowed a user's drop-in YAML when the same
// id existed in `./catalog/`. The merge fixes that.
func resolveCatalogDirs() []string {
	var out []string
	add := func(d string) {
		if d == "" {
			return
		}
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			// Avoid adding the same directory twice if the user points
			// OPOD_CATALOG_DIR at one of the other candidates.
			for _, x := range out {
				if x == d {
					return
				}
			}
			out = append(out, d)
		}
	}
	add(os.Getenv("OPOD_CATALOG_DIR"))
	add("catalog")
	if exe, err := os.Executable(); err == nil {
		add(filepath.Join(filepath.Dir(exe), "catalog"))
	}
	add("/usr/local/share/opod/catalog")
	add("/usr/share/opod/catalog")
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".opod", "catalog"))
	}
	return out
}
