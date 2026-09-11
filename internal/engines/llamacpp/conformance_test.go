package llamacpp_test

import (
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/enginetest"
)

func TestConformance(t *testing.T) {
	usage := engines.Usage{PromptTokens: 11, CompletionTokens: 4, TotalTokens: 15}
	enginetest.Run(t, enginetest.Fixture{
		Name:      "llamacpp-rpc",
		Canonical: "llamacpp",
		Backend:   enginetest.OpenAIBackend([]string{"default"}, []string{"a", "b", "c"}, usage),
		Expect: enginetest.Expect{
			Models: []string{"default"},
			Chat:   "abc",
			Usage:  &usage,
			Reason: "stop",
			Embeds: true,
		},
		NativeNames: []enginetest.NativeNameCase{
			{Source: engines.Source{ID: "qwen-15b", Repo: "Qwen/Qwen2.5-14B-Instruct-GGUF", Path: "/data/models/qwen.gguf"}, Want: "Qwen/Qwen2.5-14B-Instruct-GGUF"},
			{Source: engines.Source{ID: "local-gguf", Path: "/data/models/qwen.gguf"}, Want: "/data/models/qwen.gguf"},
		},
	})
}
