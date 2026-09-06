package models

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadCatalog_HomeDirOverridesBundled verifies the merge precedence
// documented on LoadCatalog: an entry in `~/.opod/catalog/<id>.yaml`
// overrides the same id in `./catalog/<id>.yaml`. Earlier the
// resolution stopped at the first matching directory, silently
// shadowing the user's override — that bug is what this test guards.
func TestLoadCatalog_HomeDirOverridesBundled(t *testing.T) {
	tmp := t.TempDir()
	bundled := filepath.Join(tmp, "catalog")
	home := filepath.Join(tmp, "home", ".opod", "catalog")
	if err := os.MkdirAll(bundled, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	// Both directories carry an entry with the same id but different
	// display_name; the home-dir version should win.
	bundledYAML := []byte(`
id: my-llama
display_name: Bundled My Llama
source:
  type: huggingface
  repo: example/bundled
hardware:
  min_ram_gb: 8
license: apache-2.0
`)
	homeYAML := []byte(`
id: my-llama
display_name: My Custom Llama (overrides bundled)
source:
  type: huggingface
  repo: example/custom
hardware:
  min_ram_gb: 16
license: apache-2.0
`)
	if err := os.WriteFile(filepath.Join(bundled, "my-llama.yaml"), bundledYAML, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "my-llama.yaml"), homeYAML, 0o644); err != nil {
		t.Fatal(err)
	}

	// Steer resolveCatalogDirs at our temp paths. OPOD_CATALOG_DIR
	// covers the bundled dir; HOME covers the user dir.
	t.Setenv("OPOD_CATALOG_DIR", bundled)
	t.Setenv("HOME", filepath.Join(tmp, "home"))

	entries, err := LoadCatalog("")
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	got := FindByID(entries, "my-llama")
	if got == nil {
		t.Fatal("merged catalog missing my-llama")
	}
	if got.DisplayName != "My Custom Llama (overrides bundled)" {
		t.Errorf("home-dir override did not win — DisplayName = %q", got.DisplayName)
	}
	if got.Hardware.MinRAMGB != 16 {
		t.Errorf("home-dir override did not win — MinRAMGB = %d", got.Hardware.MinRAMGB)
	}
}

// TestLoadCatalog_ExplicitDirSkipsMerge verifies the documented escape
// hatch: passing an explicit non-empty dir reads only that directory.
// Tests rely on this to keep behavior deterministic without setting
// envs.
func TestLoadCatalog_ExplicitDirSkipsMerge(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "only.yaml"), []byte(`
id: only
display_name: Only
source: {type: huggingface, repo: x/y}
hardware: {min_ram_gb: 1}
license: apache-2.0
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", "/nonexistent-home-for-this-test")
	t.Setenv("OPOD_CATALOG_DIR", "/nonexistent-dir-for-this-test")
	entries, err := LoadCatalog(tmp)
	if err != nil {
		t.Fatalf("LoadCatalog: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != "only" {
		t.Fatalf("expected [only], got %+v", entries)
	}
}

// TestParseSchemeID covers the three accepted prefixes and the fall-
// through case (a plain catalog id is not a scheme — return ok=false so
// callers fall back to catalog lookup).
func TestParseSchemeID(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantType string
		wantRepo string
		wantFile string
		wantName string
		wantPath string
	}{
		{in: "hf:Qwen/Qwen3-72B-AWQ", wantOK: true, wantType: "huggingface", wantRepo: "Qwen/Qwen3-72B-AWQ"},
		{in: "hf:bartowski/Phi-3-mini-GGUF:Phi-3-mini-4k-instruct.Q4_K_M.gguf", wantOK: true, wantType: "huggingface", wantRepo: "bartowski/Phi-3-mini-GGUF", wantFile: "Phi-3-mini-4k-instruct.Q4_K_M.gguf"},
		{in: "hf:invalid-no-slash", wantOK: false},
		{in: "hf:", wantOK: false},
		{in: "ollama:phi3", wantOK: true, wantType: "ollama", wantName: "phi3"},
		{in: "ollama:phi3:mini", wantOK: true, wantType: "ollama", wantName: "phi3:mini"},
		{in: "ollama:", wantOK: false},
		{in: "file:/tmp/x.gguf", wantOK: true, wantType: "file", wantPath: "/tmp/x.gguf"},
		{in: "file:./relative.gguf", wantOK: true, wantType: "file", wantPath: "./relative.gguf"},
		{in: "file:", wantOK: false},
		{in: "llama-3.2-3b", wantOK: false}, // plain catalog id
		{in: "", wantOK: false},
	}
	for _, c := range cases {
		e, ok := ParseSchemeID(c.in)
		if ok != c.wantOK {
			t.Errorf("%q: ok=%v want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if e.Source.Type != c.wantType {
			t.Errorf("%q: type=%q want %q", c.in, e.Source.Type, c.wantType)
		}
		if e.Source.Repo != c.wantRepo {
			t.Errorf("%q: repo=%q want %q", c.in, e.Source.Repo, c.wantRepo)
		}
		if e.Source.File != c.wantFile {
			t.Errorf("%q: file=%q want %q", c.in, e.Source.File, c.wantFile)
		}
		if e.Source.OllamaName != c.wantName {
			t.Errorf("%q: ollama_name=%q want %q", c.in, e.Source.OllamaName, c.wantName)
		}
		if e.Source.Path != c.wantPath {
			t.Errorf("%q: path=%q want %q", c.in, e.Source.Path, c.wantPath)
		}
		if e.ID != c.in {
			t.Errorf("%q: ID=%q want %q (full scheme-prefixed id should round-trip)", c.in, e.ID, c.in)
		}
	}
}
