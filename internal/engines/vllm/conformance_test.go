package vllm_test

import (
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/enginetest"
)

func TestConformance(t *testing.T) {
	usage := engines.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}
	fixture := enginetest.Fixture{
		Name:    "vllm",
		Backend: enginetest.OpenAIBackend([]string{"meta-llama/Llama-3.1-8B-Instruct", "llama-3-1-8b"}, []string{"Hel", "lo"}, usage),
		Expect: enginetest.Expect{
			Models: []string{"meta-llama/Llama-3.1-8B-Instruct", "llama-3-1-8b"},
			Chat:   "Hello",
			Usage:  &usage,
			Reason: "stop",
			Embeds: true,
		},
		NativeNames: []enginetest.NativeNameCase{
			{Source: engines.Source{ID: "llama-3-1-8b", Repo: "meta-llama/Llama-3.1-8B-Instruct", OllamaName: "llama3.1:8b"}, Want: "meta-llama/Llama-3.1-8B-Instruct"},
			{Source: engines.Source{ID: "local", Path: "/data/models/x.safetensors"}, Want: "/data/models/x.safetensors"},
			{Source: engines.Source{ID: "bare", OllamaName: "bare:latest"}, Want: "bare"},
		},
	}
	enginetest.Run(t, fixture)
}

// Tenstorrent's tt-metal server is the same OpenAI wire; the aliases must
// resolve to the vllm driver.
func TestAliases(t *testing.T) {
	for _, alias := range []string{"tt-openai", "tenstorrent", "tt"} {
		if got := engines.Canonical(alias); got != "vllm" {
			t.Errorf("Canonical(%q) = %q, want vllm", alias, got)
		}
	}
}
