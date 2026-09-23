// Package images is the manifest of the container images this repository
// builds — one engine on one accelerator per image — embedded in the binary so
// `opod image` can answer "which image serves this model on that hardware"
// without a checkout or a registry call.
//
// images.yaml is the data; build.sh and the release workflow stay what the two
// lanes execute, and cmd/opod/images_drift_test.go holds the three together.
package images

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed images.yaml
var manifestYAML []byte

// Roles, weight formats and gang schemes as images.yaml spells them.
const (
	RoleLeader = "leader"
	RoleWorker = "worker"

	WeightsGGUF        = "gguf"
	WeightsSafetensors = "safetensors"

	GangRPC = "rpc" // llama.cpp RPC parts
	GangRay = "ray" // vLLM multi-node
)

// Manifest is images.yaml.
type Manifest struct {
	Schema   int     `yaml:"schema"   json:"schema"`
	Registry string  `yaml:"registry" json:"registry"`
	Images   []Image `yaml:"images"   json:"images"`
}

// Image is one row of the manifest. The field comments in images.yaml are the
// reference; Repository is derived on load so JSON consumers need no join.
type Image struct {
	Name       string   `yaml:"name"               json:"name"`
	Repository string   `yaml:"-"                  json:"repository"`
	Role       string   `yaml:"role"               json:"role"`
	Engine     string   `yaml:"engine,omitempty"   json:"engine,omitempty"`
	Vendor     string   `yaml:"vendor,omitempty"   json:"vendor,omitempty"`
	Arch       []string `yaml:"arch"               json:"arch"`
	Dockerfile string   `yaml:"dockerfile"         json:"dockerfile"`
	Base       string   `yaml:"base"               json:"base"`
	Weights    []string `yaml:"weights,omitempty"  json:"weights,omitempty"`
	Gang       string   `yaml:"gang,omitempty"     json:"gang,omitempty"`
	Proven     bool     `yaml:"proven"             json:"proven"`
	Hardware   string   `yaml:"hardware,omitempty" json:"hardware,omitempty"`
	// Families are the GPU architecture generations this image's compiled code
	// was built for, as tokens ("amd:rdna3", "nvidia:turing,nvidia:ampere,...").
	// They are the machine-readable half of Hardware, and they exist because
	// (engine, vendor) cannot say it: upstream's ROCm vLLM ships an RDNA build
	// and a CDNA build and neither runs the other's kernels, SGLang's ROCm build
	// targets Instinct only, and vLLM's XPU build is Xe2/Xe3 while an Arc
	// A-series card is Xe-HPG. An orchestrator reads the same tokens off the
	// image's `io.opod.gpu.families` LABEL, so this file and the Dockerfile must
	// agree — a drift test checks it.
	//
	// Empty means the image says nothing, which every reader must treat as
	// unknown rather than as "no families": that is what lets the field be added
	// to one image at a time.
	Families []string `yaml:"families,omitempty" json:"families,omitempty"`
	Requires []string `yaml:"requires,omitempty" json:"requires,omitempty"`
	Summary  string   `yaml:"summary"            json:"summary"`
	Notes    []string `yaml:"notes,omitempty"    json:"notes,omitempty"`
}

// Loads reports whether the image's engine loads the given weight format.
func (i Image) Loads(format string) bool {
	for _, w := range i.Weights {
		if w == format {
			return true
		}
	}
	return false
}

var load = sync.OnceValues(func() (Manifest, error) { return parse(manifestYAML) })

// Load returns the embedded manifest.
func Load() (Manifest, error) { return load() }

func parse(data []byte) (Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // a misspelt key is a row that silently lost a fact
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("images manifest: %w", err)
	}
	if m.Schema != 1 {
		return Manifest{}, fmt.Errorf("images manifest: schema %d, this binary reads 1", m.Schema)
	}
	seen := map[string]bool{}
	for i := range m.Images {
		img := &m.Images[i]
		if err := img.validate(); err != nil {
			return Manifest{}, fmt.Errorf("images manifest: %w", err)
		}
		if seen[img.Name] {
			return Manifest{}, fmt.Errorf("images manifest: %s listed twice", img.Name)
		}
		seen[img.Name] = true
		img.Repository = m.Registry + "/" + img.Name
	}
	return m, nil
}

func (i Image) validate() error {
	switch {
	case i.Name == "" || i.Dockerfile == "" || i.Base == "" || i.Summary == "" || len(i.Arch) == 0:
		return fmt.Errorf("%q: name, dockerfile, base, arch and summary are required", i.Name)
	case i.Role == RoleLeader:
		if i.Engine != "" || i.Vendor != "" || len(i.Weights) != 0 || i.Gang != "" {
			return fmt.Errorf("%s: a leader runs no engine", i.Name)
		}
	case i.Role == RoleWorker:
		if i.Engine == "" || i.Vendor == "" || len(i.Weights) == 0 {
			return fmt.Errorf("%s: a worker names its engine, vendor and weights", i.Name)
		}
	default:
		return fmt.Errorf("%s: role %q (want %s or %s)", i.Name, i.Role, RoleLeader, RoleWorker)
	}
	for _, w := range i.Weights {
		if w != WeightsGGUF && w != WeightsSafetensors {
			return fmt.Errorf("%s: weights %q (want %s or %s)", i.Name, w, WeightsGGUF, WeightsSafetensors)
		}
	}
	if i.Gang != "" && i.Gang != GangRPC && i.Gang != GangRay {
		return fmt.Errorf("%s: gang %q (want %s or %s)", i.Name, i.Gang, GangRPC, GangRay)
	}
	return nil
}

// Find resolves an image by its name, with or without the "opod-" /
// "opod-worker-" prefix or the registry: "opod-worker-vllm-nvidia",
// "vllm-nvidia" and "ghcr.io/opod-io/opod-worker-vllm-nvidia" are one image.
func (m Manifest) Find(name string) (Image, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	name = name[strings.LastIndex(name, "/")+1:]
	for _, img := range m.Images {
		if name == img.Name || "opod-"+name == img.Name || "opod-worker-"+name == img.Name {
			return img, true
		}
	}
	return Image{}, false
}

// Filter selects images. An empty Engine or Vendor matches every image; a set
// one matches workers only, since a leader has neither.
type Filter struct {
	Engine string // canonical engine name
	Vendor string // see CanonicalVendor
}

// Select returns the images f matches, in manifest order.
func (m Manifest) Select(f Filter) []Image {
	vendor := CanonicalVendor(f.Vendor)
	var out []Image
	for _, img := range m.Images {
		if f.Engine != "" && img.Engine != f.Engine {
			continue
		}
		if vendor != "" && img.Vendor != vendor {
			continue
		}
		out = append(out, img)
	}
	return out
}

// Vendors lists the vendors that have at least one image, in manifest order.
func (m Manifest) Vendors() []string {
	return m.distinct(func(i Image) string { return i.Vendor })
}

// Engines lists the engines that have at least one image, in manifest order.
func (m Manifest) Engines() []string {
	return m.distinct(func(i Image) string { return i.Engine })
}

func (m Manifest) distinct(field func(Image) string) []string {
	var out []string
	seen := map[string]bool{}
	for _, img := range m.Images {
		if v := field(img); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// vendorAliases are other spellings a caller may hold for a manifest vendor:
// the long company name, and "none" — what a scheduler calls a node without an
// accelerator.
var vendorAliases = map[string]string{
	"tenstorrent": "tt",
	"none":        "cpu",
}

// CanonicalVendor lower-cases a vendor and resolves its aliases.
func CanonicalVendor(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	if c, ok := vendorAliases[v]; ok {
		return c
	}
	return v
}
