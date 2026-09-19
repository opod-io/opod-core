package images

import (
	"strings"
	"testing"

	"github.com/opod-io/opod-sdk/catalog"
	"github.com/opod-io/opod/internal/engines"
	_ "github.com/opod-io/opod/internal/engines/all"
	"github.com/opod-io/opod/internal/models"
)

func mustLoad(t *testing.T) Manifest {
	t.Helper()
	m, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// An engine the manifest names is an engine this binary links, by its canonical
// name — otherwise `recommend` matches a catalog entry's engines against a
// spelling nothing else in core uses.
func TestManifestEnginesAreLinkedDrivers(t *testing.T) {
	m := mustLoad(t)
	if len(m.Images) < 8 {
		t.Fatalf("only %d images parsed", len(m.Images))
	}
	for _, img := range m.Images {
		if img.Role != RoleWorker {
			continue
		}
		if d, ok := engines.Lookup(img.Engine); !ok || d.Name != img.Engine {
			t.Errorf("%s: engine %q is not a canonical linked driver (%v)", img.Name, img.Engine, engines.Names())
		}
		if !strings.HasSuffix(img.Name, "-"+img.Engine+"-"+img.Vendor) {
			t.Errorf("%s: name does not end in its engine-vendor (%s-%s)", img.Name, img.Engine, img.Vendor)
		}
	}
}

func TestOneEngineLoadsOneFormatOnEveryVendor(t *testing.T) {
	m := mustLoad(t)
	for _, engine := range m.Engines() {
		rows := m.Select(Filter{Engine: engine})
		for _, img := range rows[1:] {
			if strings.Join(img.Weights, ",") != strings.Join(rows[0].Weights, ",") {
				t.Errorf("%s loads %v but %s loads %v: Recommend reads the format off the engine's first row", img.Name, img.Weights, rows[0].Name, rows[0].Weights)
			}
		}
	}
}

func TestParseRejectsWhatWouldMislead(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown key":        "schema: 1\nregistry: r\nimages:\n  - {name: opod-leader, role: leader, arch: [amd64], dockerfile: d, base: b, summary: s, gpu: yes}\n",
		"newer schema":       "schema: 2\nregistry: r\nimages: []\n",
		"leader with engine": "schema: 1\nregistry: r\nimages:\n  - {name: opod-leader, role: leader, engine: vllm, arch: [amd64], dockerfile: d, base: b, summary: s}\n",
		"worker, no vendor":  "schema: 1\nregistry: r\nimages:\n  - {name: w, role: worker, engine: vllm, weights: [safetensors], arch: [amd64], dockerfile: d, base: b, summary: s}\n",
		"unknown gang":       "schema: 1\nregistry: r\nimages:\n  - {name: w, role: worker, engine: vllm, vendor: nvidia, weights: [safetensors], gang: mpi, arch: [amd64], dockerfile: d, base: b, summary: s}\n",
		"listed twice":       "schema: 1\nregistry: r\nimages:\n  - &l {name: opod-leader, role: leader, arch: [amd64], dockerfile: d, base: b, summary: s}\n  - *l\n",
	} {
		if _, err := parse([]byte(doc)); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}

func TestFind(t *testing.T) {
	m := mustLoad(t)
	for _, name := range []string{"opod-worker-vllm-nvidia", "vllm-nvidia", "ghcr.io/opod-io/opod-worker-vllm-nvidia", " VLLM-NVIDIA "} {
		img, ok := m.Find(name)
		if !ok || img.Name != "opod-worker-vllm-nvidia" || img.Repository != "ghcr.io/opod-io/opod-worker-vllm-nvidia" {
			t.Errorf("Find(%q) = %+v, %v", name, img.Name, ok)
		}
	}
	if img, ok := m.Find("leader"); !ok || img.Role != RoleLeader {
		t.Errorf("Find(leader) = %v, %v", img.Name, ok)
	}
	if _, ok := m.Find("vllm"); ok {
		t.Error("Find(vllm) matched: an engine is not an image")
	}
}

func picks(r Recommendation) []string {
	var out []string
	for _, c := range r.Candidates {
		if c.Recommended {
			out = append(out, c.Name)
		}
	}
	return out
}

func TestRecommend(t *testing.T) {
	m := mustLoad(t)
	hf := catalog.Source{Type: "huggingface", Repo: "org/model"}
	gguf := catalog.Source{Type: "huggingface", Repo: "org/model-GGUF", File: "model-Q4_K_M.gguf"}

	cases := []struct {
		name    string
		entry   catalog.Entry
		vendor  string
		picks   []string // the Recommended candidates, in order
		all     int      // len(Candidates)
		skipped []string // substrings, one per Skipped row, in order
	}{
		{
			name:  "safetensors goes to vLLM, proven hardware first, one pick per vendor",
			entry: catalog.Entry{ID: "m", Source: hf, RecommendedEngines: []string{"vllm"}},
			picks: []string{"opod-worker-vllm-nvidia", "opod-worker-vllm-tt", "opod-worker-vllm-amd", "opod-worker-vllm-intel"},
			all:   4,
		},
		{
			name:    "an engine the entry lists first is passed over when it cannot load the weights",
			entry:   catalog.Entry{ID: "m", Source: hf, RecommendedEngines: []string{"llamacpp", "mlx", "vllm"}},
			vendor:  "amd",
			picks:   []string{"opod-worker-vllm-amd"},
			all:     1,
			skipped: []string{"llamacpp loads gguf", "no worker image runs mlx"},
		},
		{
			name:   "a second engine is an alternative, not a second pick",
			entry:  catalog.Entry{ID: "m", Source: hf, RecommendedEngines: []string{"vllm", "sglang"}},
			vendor: "nvidia",
			picks:  []string{"opod-worker-vllm-nvidia"},
			all:    2,
		},
		{
			name:   "GGUF goes to llama.cpp; none is the CPU image",
			entry:  catalog.Entry{ID: "m", Source: gguf, RecommendedEngines: []string{"llamacpp"}},
			vendor: "none",
			picks:  []string{"opod-worker-llamacpp-cpu"},
			all:    1,
		},
		{
			name:    "an Ollama-library entry has no image",
			entry:   catalog.Entry{ID: "m", Source: catalog.Source{Type: "ollama", OllamaName: "m:8b"}, RecommendedEngines: []string{"ollama", "vllm", "llamacpp"}},
			skipped: []string{"no worker image runs ollama", "Ollama library", "Ollama library"},
		},
		{
			name: "a model split by vLLM only gets images that can join a Ray gang",
			entry: catalog.Entry{ID: "m", Source: hf, RecommendedEngines: []string{"vllm"},
				Sharding: catalog.Sharding{Required: true, Engine: "vllm"}},
			picks:   []string{"opod-worker-vllm-nvidia"},
			all:     1,
			skipped: []string{"one node only", "one node only", "one node only"},
		},
		{
			name: "a model split by llama.cpp is not offered to another engine",
			entry: catalog.Entry{ID: "m", Source: catalog.Source{Type: "file", Path: "/models/m.gguf"}, RecommendedEngines: []string{"llamacpp", "vllm"},
				Sharding: catalog.Sharding{Required: true, Engine: "llamacpp"}},
			vendor:  "tenstorrent",
			skipped: []string{"no llamacpp image is built for vendor tt", "split across workers by llamacpp"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := m.Recommend(c.entry, c.vendor)
			if got := picks(r); strings.Join(got, " ") != strings.Join(c.picks, " ") {
				t.Errorf("picks = %v, want %v", got, c.picks)
			}
			if len(r.Candidates) != c.all {
				t.Errorf("%d candidates, want %d: %+v", len(r.Candidates), c.all, r.Candidates)
			}
			if len(r.Skipped) != len(c.skipped) {
				t.Fatalf("skipped = %+v, want %d rows", r.Skipped, len(c.skipped))
			}
			for i, want := range c.skipped {
				if !strings.Contains(r.Skipped[i].Reason, want) {
					t.Errorf("skipped[%d] = %q, want it to contain %q", i, r.Skipped[i].Reason, want)
				}
			}
		})
	}
}

// Every bundled entry gets an answer an operator can act on: at least one
// image, or a reason for each engine it names.
func TestRecommendCoversTheBundledCatalog(t *testing.T) {
	m := mustLoad(t)
	entries, err := models.BundledCatalog()
	if err != nil || len(entries) == 0 {
		t.Fatalf("bundled catalog: %d entries, %v", len(entries), err)
	}
	for _, e := range entries {
		r := m.Recommend(e, "")
		if len(r.Candidates) == 0 && len(r.Skipped) != len(e.RecommendedEngines) {
			t.Errorf("%s: no image and %d reasons for %d engines", e.ID, len(r.Skipped), len(e.RecommendedEngines))
		}
		for _, c := range r.Candidates {
			if !c.Loads(r.Weights) {
				t.Errorf("%s: offered %s, which does not load %s", e.ID, c.Name, r.Weights)
			}
		}
	}
}
