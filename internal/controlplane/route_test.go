package controlplane

import (
	"reflect"
	"testing"

	"github.com/opod-io/opod/internal/api"
)

func TestDefaultRouteChain(t *testing.T) {
	pool := api.NewKeyPool()
	pool.Set("groq", []string{"k"})      // free tier, configured
	pool.Set("deepseek", []string{"k"})  // cheap, configured
	pool.Set("anthropic", []string{"k"}) // paid, configured
	// gemini/openai NOT configured → must be excluded.

	got := defaultRouteChain(pool, "qwen3.6-27b")
	want := []string{
		"groq/llama-3.3-70b-versatile",
		"deepseek/deepseek-chat",
		"claude-3-5-haiku-latest",
		"qwen3.6-27b", // local default pinned last
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("default chain = %v, want %v", got, want)
	}
}

func TestDefaultRouteChain_LocalOnly(t *testing.T) {
	// No providers configured → just the local default.
	got := defaultRouteChain(api.NewKeyPool(), "llama-3.2-1b")
	want := []string{"llama-3.2-1b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("local-only chain = %v, want %v", got, want)
	}
}

func TestDefaultRouteChain_NilPool(t *testing.T) {
	got := defaultRouteChain(nil, "qwen3.6-27b")
	if !reflect.DeepEqual(got, []string{"qwen3.6-27b"}) {
		t.Fatalf("nil-pool chain = %v, want [qwen3.6-27b]", got)
	}
}
