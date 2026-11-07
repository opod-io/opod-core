package adminapi

import "testing"

func TestEngineOverheadMiB(t *testing.T) {
	for _, c := range []struct {
		engine, vendor string
		want           int
	}{
		{"vllm", "nvidia", 837},
		{"vllm", "amd", 1103},
		{"vllm", "intel", 1103}, // not measured: the largest
		{"vllm", "", 1103},
		{"sglang", "nvidia", 0}, // no figure: the caller keeps its own
		{"llamacpp", "nvidia", 570},
		{"llamacpp", "amd", 570}, // not measured: the largest
	} {
		if got := EngineOverheadMiB(c.engine, c.vendor); got != c.want {
			t.Errorf("EngineOverheadMiB(%q, %q) = %d, want %d", c.engine, c.vendor, got, c.want)
		}
	}
}

func TestCardOverheadMiB(t *testing.T) {
	for _, c := range []struct {
		engine, vendor, card string
		want                 int
	}{
		{"llamacpp", "nvidia", "NVIDIA GeForce RTX 3060 Ti", 160},
		{"llamacpp", "nvidia", "NVIDIA GeForce RTX 5090", 570},
		{"llamacpp", "nvidia", "NVIDIA RTX A6000", 570},     // not measured: the largest
		{"llamacpp", "nvidia", "", 570},                     // card not known yet
		{"vllm", "amd", "NVIDIA GeForce RTX 3060 Ti", 1103}, // no card row: the vendor's figure
		{"sglang", "nvidia", "", 0},
	} {
		if got := CardOverheadMiB(c.engine, c.vendor, c.card); got != c.want {
			t.Errorf("CardOverheadMiB(%q, %q, %q) = %d, want %d", c.engine, c.vendor, c.card, got, c.want)
		}
	}
}
