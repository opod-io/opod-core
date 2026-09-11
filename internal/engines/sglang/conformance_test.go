package sglang_test

import (
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/enginetest"
)

// The suite every driver passes. SGLang is on the OpenAI wire, so what is
// actually being checked here is that the driver did not quietly change any of
// it — including that embeddings come back in the caller's order.
func TestConformance(t *testing.T) {
	usage := engines.Usage{PromptTokens: 7, CompletionTokens: 3, TotalTokens: 10}
	fixture := enginetest.Fixture{
		Name:    "sglang",
		Backend: enginetest.OpenAIBackend([]string{"Qwen/Qwen3-8B", "qwen3-8b"}, []string{"Hel", "lo"}, usage),
		Expect: enginetest.Expect{
			Models: []string{"Qwen/Qwen3-8B", "qwen3-8b"},
			Chat:   "Hello",
			Usage:  &usage,
			Reason: "stop",
			Embeds: true,
		},
		NativeNames: []enginetest.NativeNameCase{
			{Source: engines.Source{ID: "qwen3-8b", Repo: "Qwen/Qwen3-8B", OllamaName: "qwen3:8b"}, Want: "Qwen/Qwen3-8B"},
			{Source: engines.Source{ID: "local", Path: "/data/models/x.safetensors"}, Want: "/data/models/x.safetensors"},
			{Source: engines.Source{ID: "bare", OllamaName: "bare:latest"}, Want: "bare"},
		},
	}
	enginetest.Run(t, fixture)
}

func TestAlias(t *testing.T) {
	if got := engines.Canonical("sgl"); got != "sglang" {
		t.Errorf("Canonical(sgl) = %q, want sglang", got)
	}
}
