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
	t.Setenv("TMPDIR", t.TempDir())
	s := &Server{ModelsDir: t.TempDir(), EngineFlags: EngineFlags{"tp": "2", "max_model_len": "4096", "kv_cache_free_gpu_memory_fraction": "0.6", "extra": "--max_num_tokens 2048"}}
	got, served, err := s.trtllmCmdline("Qwen/Qwen2.5-0.5B-Instruct", "", "127.0.0.1", 8000)
	if err != nil || served != "Qwen/Qwen2.5-0.5B-Instruct" {
		t.Fatal(err, served)
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

// A pinned revision reaches trtllm-serve as --revision when it pulls the repo
// itself, and a snapshot directory is the revision — listed under the
// directory's base name, which the worker must record (read on the cell).
func TestTRTLLMPinnedRevision(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	dir := t.TempDir()
	pinned := &Server{ModelsDir: dir, ModelRevision: "0123abc"}
	got, served, err := pinned.trtllmCmdline("org/llama", "", "127.0.0.1", 8000)
	if err != nil || !strings.Contains(got, "'org/llama' --revision '0123abc' ") || served != "org/llama" {
		t.Fatalf("pinned, no snapshot: %v %q\n%s", err, served, got)
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
	got, served, err = pinned.trtllmCmdline("org/llama", "", "127.0.0.1", 8000)
	if err != nil || !strings.Contains(got, "'"+sdir+"'") || strings.Contains(got, "--revision") || served != filepath.Base(sdir) {
		t.Fatalf("snapshot: %v %q\n%s", err, served, got)
	}
}

// PLAN T27.8: trtllm-serve answered a json_schema request in prose, because
// guided decoding is an options-file setting with no flag. Every launch names
// one options file that turns it on, merged with the file the plan's extra
// names (the KV tier's), which is dropped from the line — the engine takes one.
func TestTRTLLMAlwaysTurnsOnGuidedDecoding(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	tier := filepath.Join(t.TempDir(), "tier.yaml")
	if err := os.WriteFile(tier, []byte("kv_cache_config:\n  host_cache_size: 1073741824"), 0o644); err != nil {
		t.Fatal(err)
	}
	options := filepath.Join(os.TempDir(), "opod-trtllm-options.yaml")
	for name, c := range map[string]struct {
		extra string
		want  []string
	}{
		"no file":           {"", []string{"guided_decoding_backend: xgrammar"}},
		"the tier's file":   {"--config " + tier + " --max_num_tokens 2048", []string{"host_cache_size: 1073741824\n", "guided_decoding_backend: xgrammar"}},
		"the other name":    {"--extra_llm_api_options=" + tier, []string{"host_cache_size", "guided_decoding_backend: xgrammar"}},
		"the plan's choice": {"--config " + writeOptions(t, "guided_decoding_backend: llguidance\n"), []string{"llguidance"}},
	} {
		s := &Server{EngineFlags: EngineFlags{"extra": c.extra}}
		got, _, err := s.trtllmCmdline("Qwen/Qwen2.5-0.5B-Instruct", "", "127.0.0.1", 8000)
		if err != nil {
			t.Fatal(name, err)
		}
		if strings.Count(got, "'--config'")+strings.Count(got, "--extra_llm_api_options") != 1 || !strings.Contains(got, "'--config' '"+options+"'") {
			t.Errorf("%s: want exactly one --config, the merged file:\n%s", name, got)
		}
		body, _ := os.ReadFile(options)
		for _, w := range c.want {
			if !strings.Contains(string(body), w) {
				t.Errorf("%s: options file lacks %q:\n%s", name, w, body)
			}
		}
		if name == "the plan's choice" && strings.Contains(string(body), "xgrammar") {
			t.Errorf("the plan chose a backend; it must not get a second:\n%s", body)
		}
		if name == "the tier's file" && !strings.Contains(got, "'--max_num_tokens' '2048'") {
			t.Errorf("the rest of extra must stay on the line:\n%s", got)
		}
	}
	if _, _, err := (&Server{EngineFlags: EngineFlags{"extra": "--config /nonexistent/options.yaml"}}).trtllmCmdline("m", "", "127.0.0.1", 8000); err == nil || !strings.Contains(err.Error(), "/nonexistent/options.yaml") {
		t.Errorf("an unreadable options file must fail the launch by name, got %v", err)
	}
}

func writeOptions(t *testing.T, body string) string {
	p := filepath.Join(t.TempDir(), "options.yaml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
