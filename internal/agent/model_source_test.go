package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/opod-io/opod/internal/fetch"
)

// A safetensors engine loads a prefetched snapshot only when it is complete and
// is the revision this worker pins; in every other case it keeps the repo name
// and pulls for itself, exactly as before snapshots existed.
func TestModelSourcePrefersACompleteSnapshot(t *testing.T) {
	dir := t.TempDir()
	s := &Server{ModelsDir: dir, ModelRevision: "v1"}
	if got := s.modelSource("org/llama"); got != "org/llama" {
		t.Fatalf("no snapshot: source = %q, want the repo name", got)
	}

	sdir := fetch.SnapshotDir(dir, "org/llama", "v1")
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		t.Fatal(err)
	}
	shard := filepath.Join(sdir, "model.safetensors")
	if err := os.WriteFile(shard, []byte("tensors"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.modelSource("org/llama"); got != "org/llama" {
		t.Fatalf("files without a manifest are a fetch in progress: source = %q", got)
	}

	man, _ := json.Marshal(fetch.SnapshotManifest{Repo: "org/llama", Revision: "v1",
		Files: []fetch.SnapshotFile{{Name: "model.safetensors", Size: int64(len("tensors"))}}})
	if err := os.WriteFile(filepath.Join(sdir, fetch.SnapshotManifestName), man, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.modelSource("org/llama"); got != sdir {
		t.Fatalf("complete snapshot: source = %q, want %q", got, sdir)
	}

	other := &Server{ModelsDir: dir, ModelRevision: "v2"}
	if got := other.modelSource("org/llama"); got != "org/llama" {
		t.Fatalf("a worker pinned to v2 must not serve the v1 snapshot: source = %q", got)
	}
	if err := os.Remove(shard); err != nil {
		t.Fatal(err)
	}
	if got := s.modelSource("org/llama"); got != "org/llama" {
		t.Fatalf("a snapshot that lost a shard: source = %q, want the repo name", got)
	}
}
