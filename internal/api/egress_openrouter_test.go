package api

import (
	"encoding/json"
	"testing"
)

func TestVendor_OpenAIGateways(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"openrouter/anthropic/claude-3-haiku", "openrouter"},
		{"openrouter/meta-llama/llama-3.1-70b", "openrouter"},
		{"groq/llama-3.1-70b-versatile", "groq"},
		{"together/Qwen/Qwen2.5-72B-Instruct-Turbo", "together"},
		{"fireworks/accounts/fireworks/models/llama-v3p1-70b-instruct", "fireworks"},
		{"cohere/command-r-plus", "cohere"},
		{"mistral/mistral-large-latest", "mistral"},
		{"perplexity/sonar-pro", "perplexity"},

		// Existing routes must keep working.
		{"claude-3-5-sonnet", "anthropic"},
		{"gpt-4o", "openai"},
		{"gemini-1.5-pro", "vertex"},
		{"anthropic.claude-3-sonnet", "bedrock"},

		// Bare local id — no vendor.
		{"qwen3-14b", ""},
		{"llama-3.2-3b", ""},
	}
	for _, c := range cases {
		t.Run(c.model, func(t *testing.T) {
			if got := Vendor(c.model); got != c.want {
				t.Errorf("Vendor(%q) = %q, want %q", c.model, got, c.want)
			}
		})
	}
}

func TestStripModelPrefix(t *testing.T) {
	in := []byte(`{"model":"openrouter/anthropic/claude-3-haiku","messages":[{"role":"user","content":"hi"}]}`)
	got := stripModelPrefix(in, "openrouter/")
	var obj map[string]any
	if err := json.Unmarshal(got, &obj); err != nil {
		t.Fatalf("decode rewritten: %v", err)
	}
	if obj["model"] != "anthropic/claude-3-haiku" {
		t.Errorf("model = %v, want anthropic/claude-3-haiku", obj["model"])
	}
	if obj["messages"] == nil {
		t.Error("messages must survive the rewrite")
	}
}

func TestStripModelPrefix_NoMatch(t *testing.T) {
	// Body that doesn't start with the prefix returns unchanged.
	in := []byte(`{"model":"qwen3-14b","messages":[]}`)
	got := stripModelPrefix(in, "openrouter/")
	if string(got) != string(in) {
		t.Errorf("non-prefix body should be unchanged; got %q", got)
	}
}

func TestStripModelPrefix_InvalidJSON(t *testing.T) {
	in := []byte(`{not really json`)
	got := stripModelPrefix(in, "openrouter/")
	if string(got) != string(in) {
		t.Errorf("invalid JSON should pass through unchanged")
	}
}
