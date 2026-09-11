package mlx_test

import (
	"testing"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/engines/enginetest"
)

func TestConformance(t *testing.T) {
	usage := engines.Usage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7}
	enginetest.Run(t, enginetest.Fixture{
		Name:      "mlx-lm", // construct through the alias; Name() must still be canonical
		Canonical: "mlx",
		Backend:   enginetest.OpenAIBackend([]string{"mlx-community/Qwen2.5-Coder-14B-Instruct-4bit"}, []string{"ok"}, usage),
		Expect: enginetest.Expect{
			Models: []string{"mlx-community/Qwen2.5-Coder-14B-Instruct-4bit"},
			Chat:   "ok",
			Usage:  &usage,
			Reason: "stop",
			Embeds: true,
		},
		NativeNames: []enginetest.NativeNameCase{
			{Source: engines.Source{ID: "qwen-coder-14b", Repo: "mlx-community/Qwen2.5-Coder-14B-Instruct-4bit"}, Want: "mlx-community/Qwen2.5-Coder-14B-Instruct-4bit"},
			{Source: engines.Source{ID: "bare"}, Want: "bare"},
		},
	})
}
