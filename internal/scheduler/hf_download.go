package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/opod-io/opod/internal/fetch"
	"github.com/opod-io/opod/internal/models"
)

// ensureLocalGGUF resolves the local filesystem path of a sharded model's
// GGUF, downloading from HuggingFace when needed. Returns the absolute path
// to the file on the leader's disk; caller hands that to
// ensureGGUFOnAllWorkers for downstream distribution.
//
// Three input shapes:
//
//	source.type: file         → return source.path verbatim, just check it exists
//	source.type: huggingface  → download to <modelsDir>/<source.file>, return
//	                            that path. Skip download if already present.
//	other                     → error (Bedrock / Vertex / Ollama don't apply
//	                            to sharded entries)
//
// The download is a single streaming GET to huggingface.co/<repo>/resolve/main/<file>
// with a generous timeout — GGUFs can be 10s of GB. Resume support is left
// for a follow-up; for now an interrupted download leaves the .partial file
// behind and the next call starts over.
func (o *Orchestrator) ensureLocalGGUF(ctx context.Context, entry models.Entry) (string, error) {
	switch entry.Source.Type {
	case "file":
		if entry.Source.Path == "" {
			return "", fmt.Errorf("catalog %s: source.type=file requires source.path", entry.ID)
		}
		if _, err := os.Stat(entry.Source.Path); err != nil {
			return "", fmt.Errorf("catalog %s: source.path %q not present and source.type=file (place the GGUF manually or change source.type to huggingface for auto-download)", entry.ID, entry.Source.Path)
		}
		return entry.Source.Path, nil

	case "huggingface":
		if entry.Source.Repo == "" || entry.Source.File == "" {
			return "", fmt.Errorf("catalog %s: source.type=huggingface requires both source.repo and source.file for auto-download", entry.ID)
		}
		// source.file is joined into ModelsDir and interpolated into the
		// download URL, so a path-y value could escape either. Mirror the
		// basename-only rule the worker upload endpoint enforces.
		if f := entry.Source.File; f == "." || strings.Contains(f, "..") ||
			strings.ContainsAny(f, "/\\") || filepath.Base(f) != f {
			return "", fmt.Errorf("catalog %s: source.file %q must be a bare filename (no path separators or ..)", entry.ID, f)
		}
		if o.ModelsDir == "" {
			return "", fmt.Errorf("catalog %s wants HF auto-download but Orchestrator.ModelsDir is unset; configure storage.models_dir or pre-place at source.path with type=file", entry.ID)
		}
		// One fetch for every caller (models.FetchGGUF): present with the
		// declared size = skipped, another size = pulled again, another
		// process pulling = waited for; temp + rename, never a partial file
		// under the final name.
		return fetch.GGUF(ctx, entry.Source.Repo, entry.Source.File, o.ModelsDir, fetch.Options{Log: o.Log, Token: os.Getenv("HF_TOKEN")})

	default:
		return "", fmt.Errorf("catalog %s: source.type=%q can't be auto-resolved for sharding (need file or huggingface)", entry.ID, entry.Source.Type)
	}
}
