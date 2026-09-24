package main

import (
	"testing"

	"github.com/opod-io/opod/internal/models"
)

// The page for a catalog entry's weights. A repo string names the bytes but is
// not somewhere an operator can go and look, and for a GGUF the FILE is what
// says which quantization is served — the model id never does.
func TestSourceURL(t *testing.T) {
	hf := func(repo, file string) *models.Entry {
		e := &models.Entry{}
		e.Source.Type = "huggingface"
		e.Source.Repo, e.Source.File = repo, file
		return e
	}
	for _, tc := range []struct {
		name  string
		entry *models.Entry
		want  string
	}{
		{"repo and file", hf("unsloth/GLM-4.5-Air-GGUF", "GLM-4.5-Air-UD-Q2_K_XL.gguf"),
			"https://huggingface.co/unsloth/GLM-4.5-Air-GGUF/blob/main/GLM-4.5-Air-UD-Q2_K_XL.gguf"},
		{"repo only", hf("zai-org/GLM-4.6", ""), "https://huggingface.co/zai-org/GLM-4.6"},
		{"hub entry with no repo", hf("", "x.gguf"), ""},
		{"a file staged on the node has no upstream", func() *models.Entry {
			e := &models.Entry{}
			e.Source.Type = "file"
			e.Source.Path = "/var/lib/opod/models/x.gguf"
			return e
		}(), ""},
		{"ollama drops the tag", func() *models.Entry {
			e := &models.Entry{}
			e.Source.Type = "ollama"
			e.Source.OllamaName = "glm4:9b"
			return e
		}(), "https://ollama.com/library/glm4"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceURL(tc.entry); got != tc.want {
				t.Fatalf("sourceURL = %q, want %q", got, tc.want)
			}
		})
	}
}
