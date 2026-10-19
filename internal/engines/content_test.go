package engines

import (
	"encoding/json"
	"reflect"
	"testing"
)

// ParseContent takes a string or content parts; an image keeps its whole URL,
// so the media type of a data URL reaches an OpenAI-shaped engine (T16.2).
func TestParseContent(t *testing.T) {
	if text, images := ParseContent(json.RawMessage(`"hello world"`)); text != "hello world" || images != nil {
		t.Fatalf("string: %q %v", text, images)
	}
	text, images := ParseContent(json.RawMessage(`[
		{"type":"text","text":"line one"},
		{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0aGgo"}},
		{"type":"text","text":"line two"},
		{"type":"image_url","image_url":{"url":"https://example.com/cat.jpg"}}]`))
	if text != "line one line two" || !reflect.DeepEqual(images, []string{"data:image/png;base64,iVBORw0aGgo", "https://example.com/cat.jpg"}) {
		t.Fatalf("parts: %q %v", text, images)
	}
	if text, images := ParseContent(nil); text != "" || images != nil {
		t.Fatalf("nil: %q %v", text, images)
	}
}

// ContentValue is the text alone without images — a text-only body is exactly
// what it was — and content parts with them.
func TestContentValue(t *testing.T) {
	if v := ContentValue("hi", nil); v != "hi" {
		t.Fatalf("text only: %#v", v)
	}
	raw, _ := json.Marshal(ContentValue("what is this", []string{"data:image/png;base64,AAA"}))
	want := `[{"text":"what is this","type":"text"},{"image_url":{"url":"data:image/png;base64,AAA"},"type":"image_url"}]`
	if string(raw) != want {
		t.Fatalf("parts:\n got %s\nwant %s", raw, want)
	}
}

func TestStripDataURL(t *testing.T) {
	for in, want := range map[string]string{
		"data:image/png;base64,iVBORw0aGgo": "iVBORw0aGgo",
		"https://example.com/photo.jpg":     "https://example.com/photo.jpg",
	} {
		if got := StripDataURL(in); got != want {
			t.Errorf("StripDataURL(%q) = %q, want %q", in, got, want)
		}
	}
}
