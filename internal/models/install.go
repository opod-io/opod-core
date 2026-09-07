package models

// Install-time model logic shared by the CLI (`opod model add`) and anything
// else that installs a catalog entry on this host: source/engine
// compatibility, the user-catalog copy for `--from`, catalog search, and the
// pull + registry/placement upsert itself. The CLI only parses flags and
// prints what these return (P13-9).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// SourceCompatibleWithEngine reports whether a catalog (or synthetic) source
// type can be served by the named engine — a pre-flight guard so the user
// gets "switch engines" instead of a cryptic engine-side 404. An empty source
// type (older catalog entries) is treated as compatible; the engine decides.
func SourceCompatibleWithEngine(sourceType, engineName string) bool {
	switch sourceType {
	case "", "auto":
		return true
	case "ollama":
		return engineName == "ollama"
	case "huggingface":
		return engineName == "vllm" || engineName == "mlx" || engineName == "mlx-lm" ||
			engineName == "tt-openai" || engineName == "tenstorrent" || engineName == "tt" ||
			strings.HasPrefix(engineName, "llamacpp") || strings.HasPrefix(engineName, "llama-cpp")
	case "file":
		return engineName == "mlx" || engineName == "mlx-lm" ||
			strings.HasPrefix(engineName, "llamacpp") || strings.HasPrefix(engineName, "llama-cpp")
	}
	return true
}

// PersistUserCatalogEntry writes a user-supplied catalog YAML into the user
// catalog dir (configuredDir, else ~/.opod/catalog) as <id>.yaml so it shows
// up in search/info next run. Never overwrites: an existing entry with the
// same id is an error the caller surfaces. Returns the destination path.
func PersistUserCatalogEntry(configuredDir, id string, data []byte) (string, error) {
	dir := configuredDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		dir = filepath.Join(home, ".opod", "catalog")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	dest := filepath.Join(dir, id+".yaml")
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("%s already exists — remove or rename it first", dest)
	}
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", dest, err)
	}
	return dest, nil
}

// Search filters the catalog by a free-text query (id, display name,
// capability or tag, case-insensitive), an optional `since` release date
// (YYYY-MM-DD, entries without a date are excluded) and optionally sorts
// newest-first (undated entries sink to the bottom, smaller first).
func Search(cat []Entry, query, since string, sortReleased bool) []Entry {
	q := strings.ToLower(query)
	rows := make([]Entry, 0, len(cat))
	for _, e := range cat {
		if q != "" &&
			!strings.Contains(strings.ToLower(e.ID), q) &&
			!strings.Contains(strings.ToLower(e.DisplayName), q) &&
			!containsAny(e.Capabilities, q) &&
			!containsAny(e.Tags, q) {
			continue
		}
		if since != "" && (e.Released == "" || e.Released < since) {
			continue
		}
		rows = append(rows, e)
	}
	if sortReleased {
		sort.SliceStable(rows, func(i, j int) bool {
			a, b := rows[i].Released, rows[j].Released
			if a == "" && b == "" {
				return rows[i].SizeBytes < rows[j].SizeBytes
			}
			if a == "" {
				return false
			}
			if b == "" {
				return true
			}
			return a > b
		})
	}
	return rows
}

func containsAny(items []string, q string) bool {
	for _, it := range items {
		if strings.Contains(strings.ToLower(it), q) {
			return true
		}
	}
	return false
}

// Install errors the CLI turns into specific messages.
var (
	// ErrIncompatibleSource: the entry's source scheme has no path through this engine.
	ErrIncompatibleSource = errors.New("source type not compatible with engine")
	// ErrNoNativeName: the entry carries no source name this engine can pull.
	ErrNoNativeName = errors.New("no engine-native source name")
)

// RegisterError means the weights were pulled but recording the install (the
// model row or its placement) failed; a rerun registers it once the store is
// healthy.
type RegisterError struct {
	What string // "model" | "placement"
	Err  error
}

func (e *RegisterError) Error() string { return "record " + e.What + ": " + e.Err.Error() }
func (e *RegisterError) Unwrap() error { return e.Err }

// Install pulls a resolved entry through the engine and records it in the
// local registry + placements so the router can find it. It does not run the
// hardware floor, the source probe or sharding delegation — those are policy
// the caller applies first. onProgress receives the engine's pull progress.
// Returns the engine-native name that was pulled.
func Install(ctx context.Context, st store.Store, eng engines.Engine, entry *Entry, onProgress func(status string, completed, total int64)) (string, error) {
	if !SourceCompatibleWithEngine(entry.Source.Type, eng.Name()) {
		return "", fmt.Errorf("%w: %q in %s vs engine %s", ErrIncompatibleSource, entry.Source.Type, entry.ID, eng.Name())
	}
	engineName := EngineNativeName(eng.Name(), entry)
	if engineName == "" {
		return "", fmt.Errorf("%w: %s on %s", ErrNoNativeName, entry.ID, eng.Name())
	}
	if err := eng.Health(ctx); err != nil {
		return engineName, fmt.Errorf("engine not reachable: %w", err)
	}
	if err := eng.Pull(ctx, engineName, onProgress); err != nil {
		return engineName, fmt.Errorf("pull: %w", err)
	}
	if err := st.Models().Upsert(ctx, store.Model{
		ID:          entry.ID,
		CatalogID:   entry.ID,
		Source:      eng.Name() + ":" + engineName,
		Status:      "ready",
		SizeBytes:   entry.SizeBytes,
		InstalledAt: time.Now(),
	}); err != nil {
		return engineName, &RegisterError{What: "model", Err: err}
	}
	// This node's placement, so the router can find the model; on a worker
	// the leader reconciles the row from the next heartbeat too.
	if err := st.Placements().Upsert(ctx, store.Placement{
		NodeID:   "local",
		ModelID:  engineName,
		Status:   "ready",
		LastSeen: time.Now(),
	}); err != nil {
		return engineName, &RegisterError{What: "placement", Err: err}
	}
	return engineName, nil
}
