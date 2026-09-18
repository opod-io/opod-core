package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// A worker that pins a revision and has no snapshot of it lets the engine pull
// — and the engine must be told which revision, or it serves the branch head.
// A snapshot directory is the revision already; an unpinned worker passes
// nothing; a revision that cannot be quoted into the launch refuses it.
func TestPinnedRevisionReachesTheEngine(t *testing.T) {
	launchers := map[string]func(*Server) (string, error){
		"vllm":   func(s *Server) (string, error) { return s.vllmCmdline("org/llama", "llama", "127.0.0.1", 8000) },
		"sglang": func(s *Server) (string, error) { return s.sglangCmdline("org/llama", "llama", "127.0.0.1", 30000) },
	}
	for name, cmdline := range launchers {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pinned := &Server{ModelsDir: dir, ModelRevision: "0123abc"}
			got, err := cmdline(pinned)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, "'org/llama' --revision '0123abc' ") {
				t.Fatalf("pinned, no snapshot: the engine is not told the revision:\n%s", got)
			}

			got, err = cmdline(&Server{ModelsDir: dir})
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(got, "--revision") {
				t.Fatalf("unpinned: --revision must not appear:\n%s", got)
			}

			sdir := fetch.SnapshotDir(dir, "org/llama", "0123abc")
			if err := os.MkdirAll(sdir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sdir, "model.safetensors"), []byte("tensors"), 0o644); err != nil {
				t.Fatal(err)
			}
			man, _ := json.Marshal(fetch.SnapshotManifest{Repo: "org/llama", Revision: "0123abc",
				Files: []fetch.SnapshotFile{{Name: "model.safetensors", Size: int64(len("tensors"))}}})
			if err := os.WriteFile(filepath.Join(sdir, fetch.SnapshotManifestName), man, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err = cmdline(pinned)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, "'"+sdir+"'") || strings.Contains(got, "--revision") {
				t.Fatalf("a snapshot is the revision: want the directory and no --revision:\n%s", got)
			}

			if _, err := cmdline(&Server{ModelsDir: dir, ModelRevision: "v1/../../etc"}); err == nil {
				t.Fatal("a revision that is not a sha, tag or branch must refuse the launch")
			}
			got, err = cmdline(&Server{ModelsDir: dir, ModelRevision: "v1';id;'"})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(got, `--revision 'v1'\'';id;'\'''`) {
				t.Fatalf("the revision must be shell-quoted:\n%s", got)
			}
		})
	}
}
