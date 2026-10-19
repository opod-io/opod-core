package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// An image reaches an OpenAI-shaped engine as a content part, its data URL
// whole; a text-only message is the plain string it always was (T16.2).
func TestBodyCarriesImagesAsContentParts(t *testing.T) {
	raw, _ := json.Marshal(BuildChatBody(engines.ChatRequest{Model: "m", Messages: []engines.Message{
		{Role: "user", Content: "what is this", Images: []string{"data:image/png;base64,AAA"}},
		{Role: "assistant", Content: "a square"},
	}}))
	if !strings.Contains(string(raw), `"content":[{"text":"what is this","type":"text"},{"image_url":{"url":"data:image/png;base64,AAA"},"type":"image_url"}]`) {
		t.Errorf("image turn: %s", raw)
	}
	if !strings.Contains(string(raw), `"content":"a square"`) {
		t.Errorf("text turn changed: %s", raw)
	}
}

// A client that says it cannot take images refuses one by name before any
// request leaves — the drivers of SGLang, llama.cpp and MLX set it.
func TestNoImagesRefusesByName(t *testing.T) {
	c := NewClient("sglang", "http://127.0.0.1:1", nil)
	c.NoImages = "not proven here"
	_, err := c.Chat(context.Background(), engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: "x", Images: []string{"data:image/png;base64,AAA"}}}})
	var unsup *engines.UnsupportedRequestError
	if !errors.As(err, &unsup) || unsup.Field != "image_url" {
		t.Fatalf("got %v, want an UnsupportedRequestError naming image_url", err)
	}
}
