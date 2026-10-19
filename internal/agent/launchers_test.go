package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/fetch"
)

// Each launched engine is one row of the table (T19.15).
func TestLauncherFor(t *testing.T) {
	for engine, process := range map[string]string{"vllm": "vllm-serve", "sglang": "sglang-serve", "trtllm": "trtllm-serve"} {
		if l, ok := launcherFor(engine); !ok || l.process != process {
			t.Errorf("%s: %+v %v", engine, l, ok)
		}
	}
	if _, ok := launcherFor("llamacpp"); ok {
		t.Error("llama.cpp is launched by its own path, not this table")
	}
}

// trtllm-serve runs on the PyTorch backend with the plan's flags under their
// own names, and always states its slot count (ADR-091).
func TestTRTLLMCmdline(t *testing.T) {
	s := &Server{ModelsDir: t.TempDir(), EngineFlags: EngineFlags{"tp": "2", "max_model_len": "4096", "kv_cache_free_gpu_memory_fraction": "0.6", "extra": "--max_num_tokens 2048"}}
	got, err := s.trtllmCmdline("Qwen/Qwen2.5-0.5B-Instruct", "127.0.0.1", 8000)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TP=2;", "exec trtllm-serve 'Qwen/Qwen2.5-0.5B-Instruct' --host 127.0.0.1 --port 8000 --backend pytorch --tp_size \"$TP\"",
		"'--max_batch_size' '64'", "'--max_seq_len' '4096'", "'--kv_cache_free_gpu_memory_fraction' '0.60'", "'--max_num_tokens' '2048'"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if n := s.EngineFlags.LaunchSlots("trtllm"); n != 64 {
		t.Errorf("slots = %d, want the default 64", n)
	}
}

// trtllm-serve has no --revision: a pinned revision is served from its
// snapshot, and refused without one rather than serving the branch head.
func TestTRTLLMPinnedRevision(t *testing.T) {
	dir := t.TempDir()
	pinned := &Server{ModelsDir: dir, ModelRevision: "0123abc"}
	if _, err := pinned.trtllmCmdline("org/llama", "127.0.0.1", 8000); err == nil || !strings.Contains(err.Error(), "prefetch") {
		t.Fatalf("pinned, no snapshot: %v", err)
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
	got, err := pinned.trtllmCmdline("org/llama", "127.0.0.1", 8000)
	if err != nil || !strings.Contains(got, "'"+sdir+"'") {
		t.Fatalf("snapshot: %v\n%s", err, got)
	}
}
