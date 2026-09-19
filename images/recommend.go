package images

import (
	"fmt"
	"strings"

	"github.com/opod-io/opod-sdk/catalog"
)

// Recommendation answers "which image serves this catalog model", per vendor.
// It is a lookup over two lists the repository already keeps — the entry's
// recommended_engines and the manifest — never a measurement: whether the
// model FITS a given card is the entry's hardware block and the operator's call.
type Recommendation struct {
	Model   string   `json:"model"`
	Weights string   `json:"weights"`            // gguf | safetensors | ollama
	Engines []string `json:"engines"`            // the entry's recommended_engines, in its order
	Sharded bool     `json:"sharded"`            // the entry must be split across workers
	ShardBy string   `json:"shard_by,omitempty"` // the engine that splits it
	// Candidates are ordered: the entry's engine order first, images proven on
	// hardware before unproven ones. The first candidate of each vendor has
	// Recommended set.
	Candidates []Candidate `json:"candidates"`
	Skipped    []Skipped   `json:"skipped,omitempty"`
	MinVRAMGB  int         `json:"min_vram_gb,omitempty"`
	MinRAMGB   int         `json:"min_ram_gb,omitempty"`
}

// Candidate is one image that can serve the model.
type Candidate struct {
	Image
	Recommended bool `json:"recommended"`
}

// Skipped is an engine or image the entry names that cannot serve it, and why.
type Skipped struct {
	Engine string `json:"engine"`
	Image  string `json:"image,omitempty"`
	Reason string `json:"reason"`
}

// WeightsOf is the format an entry's source resolves to: a GGUF file, a
// safetensors Hugging Face repo, or the Ollama library (which no image runs).
func WeightsOf(e catalog.Entry) string {
	isGGUF := func(s string) bool { return strings.HasSuffix(strings.ToLower(s), ".gguf") }
	switch {
	case isGGUF(e.Source.File) || isGGUF(e.Source.Path):
		return WeightsGGUF
	case e.Source.Repo != "" && e.Source.File == "":
		return WeightsSafetensors
	case e.Source.OllamaName != "":
		return "ollama"
	}
	return ""
}

// Recommend lists the images that can serve e. vendor narrows the answer to one
// accelerator ("" = every vendor).
func (m Manifest) Recommend(e catalog.Entry, vendor string) Recommendation {
	rec := Recommendation{
		Model:     e.ID,
		Weights:   WeightsOf(e),
		Engines:   e.RecommendedEngines,
		Sharded:   e.Sharding.Required,
		MinVRAMGB: e.Hardware.MinVRAMGB,
		MinRAMGB:  e.Hardware.MinRAMGB,
	}
	if rec.Sharded {
		rec.ShardBy = e.Sharding.Engine
	}
	skip := func(engine, image, format string, args ...any) {
		rec.Skipped = append(rec.Skipped, Skipped{Engine: engine, Image: image, Reason: fmt.Sprintf(format, args...)})
	}

	picked := map[string]bool{} // vendor → already has its recommended image
	for _, engine := range e.RecommendedEngines {
		all := m.Select(Filter{Engine: engine})
		if len(all) == 0 {
			skip(engine, "", "no worker image runs %s; it serves on a host started with `opod up`", engine)
			continue
		}
		if rec.Sharded && engine != rec.ShardBy {
			skip(engine, "", "the entry is split across workers by %s", rec.ShardBy)
			continue
		}
		// One engine loads one format on every vendor, so the first row speaks for it.
		if !all[0].Loads(rec.Weights) {
			skip(engine, "", "%s loads %s; this entry's weights are %s", engine, strings.Join(all[0].Weights, " or "), describeWeights(rec.Weights))
			continue
		}
		images := m.Select(Filter{Engine: engine, Vendor: vendor})
		if len(images) == 0 {
			skip(engine, "", "no %s image is built for vendor %s", engine, CanonicalVendor(vendor))
			continue
		}
		// Proven images first, manifest order otherwise.
		for _, proven := range []bool{true, false} {
			for _, img := range images {
				if img.Proven != proven {
					continue
				}
				if rec.Sharded && img.Gang == "" {
					skip(engine, img.Name, "the entry must be split across workers and this image serves one node only")
					continue
				}
				rec.Candidates = append(rec.Candidates, Candidate{Image: img, Recommended: !picked[img.Vendor]})
				picked[img.Vendor] = true
			}
		}
	}
	return rec
}

func describeWeights(format string) string {
	switch format {
	case WeightsGGUF:
		return "a GGUF file"
	case WeightsSafetensors:
		return "a safetensors Hugging Face repo"
	case "ollama":
		return "in the Ollama library, which no image pulls (use a GGUF or Hugging Face entry of the model, or hf:<owner>/<repo>)"
	}
	return "not something an image can pull (no Hugging Face repo or GGUF path)"
}
