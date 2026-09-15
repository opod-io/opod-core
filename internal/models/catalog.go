// Package models loads catalog entries (YAML files describing known models)
// and implements model-selection helpers.
package models

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/opod-io/opod-sdk/catalog"
	"github.com/opod-io/opod/internal/engines"
	"gopkg.in/yaml.v3"
)

// Entry is a single catalog entry, loaded from catalog/<id>.yaml.
// The catalog schema lives in the SDK (opod-io/opod-sdk/catalog, R9.8): one
// set of files and one set of types that the leader and the control plane
// both read. The names below are the ones this package always used.
type (
	Entry            = catalog.Entry
	SourceSpec       = catalog.Source
	ShardingSpec     = catalog.Sharding
	ArchitectureSpec = catalog.Architecture
	HardwareSpec     = catalog.Hardware
)

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
//  1. ./catalog (relative to cwd)              (bundled — least authoritative)
//  2. <exe-dir>/catalog
//  3. /usr/local/share/opod/catalog
//  4. /usr/share/opod/catalog
//  5. $OPOD_CATALOG_DIR        (the catalog the operator points at — wins over the bundled tree)
//  6. ~/.opod/catalog          (most authoritative — user overrides)
//
// $OPOD_CATALOG_DIR sat first (least authoritative) until 2026-09-14: an
// entry there that reused a bundled id was silently shadowed by the bundled
// one, so a leader given a sharded entry for a catalog model answered "not
// configured for sharding". Whoever names the directory means it.
//
// An explicit non-empty dir argument skips the merge and reads only
// that directory (used by tests and callers that know exactly what they
// want).
//
// Entries are returned sorted by SizeBytes ascending on every path —
// AutoPick (and anything else scanning for "largest that fits") relies
// on that invariant.
// overrides are extra directories (OPOD_CATALOG_DIR through the config
// contract) read after the share directories and before ~/.opod/catalog;
// ignored when dir names one directory to read alone.
func LoadCatalog(dir string, overrides ...string) ([]Entry, error) {
	var out []Entry
	if dir != "" {
		got, err := readCatalogDir(dir)
		if err != nil {
			return nil, err
		}
		out = got
	} else {
		// The bundled set is the SDK's embedded catalog (R9.8) — always
		// present, never a directory the binary has to find. Directories
		// override it: later ones overwrite earlier ones, keyed by id. We do
		// not warn about a shadowed entry — the effective entry is what the
		// listing shows, which is what matters operationally.
		merged := map[string]Entry{}
		bundled, err := BundledCatalog()
		if err != nil {
			return nil, err
		}
		for _, e := range bundled {
			merged[e.ID] = e
		}
		for _, d := range resolveCatalogDirs(overrides) {
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

// BundledCatalog parses the SDK's embedded catalog — what `opod` knows with
// no directory at all.
func BundledCatalog() ([]Entry, error) {
	names := catalog.Names()
	out := make([]Entry, 0, len(names))
	for _, name := range names {
		data, err := catalog.Read(name)
		if err != nil {
			return nil, fmt.Errorf("bundled catalog %s: %w", name, err)
		}
		var e Entry
		if err := yaml.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("bundled catalog %s: %w", name, err)
		}
		if e.ID == "" {
			return nil, fmt.Errorf("bundled catalog %s: missing id", name)
		}
		out = append(out, e)
	}
	return out, nil
}

// ExportBundled writes the embedded catalog as files into dir (one per
// entry), the shape the worker entrypoint and an operator's overrides read.
func ExportBundled(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range catalog.Names() {
		data, err := catalog.Read(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
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
func resolveCatalogDirs(overrides []string) []string {
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
	// Overrides only — the bundled set is embedded (BundledCatalog). The
	// share directories stay for an operator's drop-in files and for the
	// worker entrypoint's exported copy (identical content, harmless).
	add("/usr/local/share/opod/catalog")
	add("/usr/share/opod/catalog")
	for _, d := range overrides { // explicit: beats the bundled tree, yields to ~/.opod/catalog
		add(d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(filepath.Join(home, ".opod", "catalog"))
	}
	return out
}
