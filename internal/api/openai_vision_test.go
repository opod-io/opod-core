package api

import (
	"encoding/json"
	"testing"
)

// parseChatContent must accept both legacy string content and the OpenAI
// multimodal array shape, and must strip the data-URL prefix from inline
// base64 images so engines receive a bare payload.
func TestParseChatContent_String(t *testing.T) {
	text, images := parseChatContent(json.RawMessage(`"hello world"`))
	if text != "hello world" {
		t.Fatalf("text = %q, want %q", text, "hello world")
	}
	if len(images) != 0 {
		t.Fatalf("images = %v, want none", images)
	}
}

func TestParseChatContent_MultimodalArray(t *testing.T) {
	raw := json.RawMessage(`[
		{"type":"text","text":"what is in this image"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0aGgo"}},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}}
	]`)
	text, images := parseChatContent(raw)
	if text != "what is in this image" {
		t.Fatalf("text = %q, want %q", text, "what is in this image")
	}
	if len(images) != 2 {
		t.Fatalf("len(images) = %d, want 2", len(images))
	}
	if images[0] != "iVBORw0aGgo" {
		t.Fatalf("images[0] = %q, want base64 with data: prefix stripped", images[0])
	}
	if images[1] != "https://example.com/cat.jpg" {
		t.Fatalf("images[1] = %q, want raw https URL untouched", images[1])
	}
}

func TestParseChatContent_TextPartsConcatenated(t *testing.T) {
	// Two text parts in a row should be joined with a single space — preserves
	// caller intent without inserting extra whitespace that confuses tokenizers.
	raw := json.RawMessage(`[
		{"type":"text","text":"line one"},
		{"type":"text","text":"line two"}
	]`)
	text, _ := parseChatContent(raw)
	if text != "line one line two" {
		t.Fatalf("text = %q, want %q", text, "line one line two")
	}
}

func TestParseChatContent_Empty(t *testing.T) {
	text, images := parseChatContent(nil)
	if text != "" || len(images) != 0 {
		t.Fatalf("nil raw should produce empty result, got text=%q images=%v", text, images)
	}
}

func TestStripDataURLPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"data:image/png;base64,iVBORw0aGgo", "iVBORw0aGgo"},
		{"data:image/jpeg;base64,/9j/4AAQSkZ", "/9j/4AAQSkZ"},
		{"https://example.com/photo.jpg", "https://example.com/photo.jpg"},
		{"plain-string", "plain-string"},
	}
	for _, c := range cases {
		if got := stripDataURLPrefix(c.in); got != c.want {
			t.Errorf("stripDataURLPrefix(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
