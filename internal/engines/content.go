package engines

// A chat message's content on the OpenAI wire is a string, or a list of content
// parts — text and `image_url` — for a vision model (PLAN T16.2). The gateway
// parses it, the leader re-sends it to a worker, and the worker hands it to its
// engine; every hop uses these helpers, so an image survives all three. Before
// them the gateway parsed images, the shared serialiser sent text only and the
// worker decoded a string, so no engine behind a worker ever saw one.

import (
	"encoding/json"
	"strings"
)

// ParseContent reads a message's content: a JSON string, or OpenAI content
// parts. Text parts are joined with a single space; each `image_url` part is
// kept as its URL, a data URL whole — its media type is part of it, and an
// OpenAI-shaped engine needs it back. Anything else parses as empty text: the
// engine says clearly when a prompt is empty.
func ParseContent(raw json.RawMessage) (text string, images []string) {
	if len(raw) == 0 {
		return "", nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if json.Unmarshal(raw, &parts) != nil {
		return "", nil
	}
	var b strings.Builder
	for _, p := range parts {
		switch p.Type {
		case "text":
			if b.Len() > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(p.Text)
		case "image_url":
			if p.ImageURL != nil && p.ImageURL.URL != "" {
				images = append(images, p.ImageURL.URL)
			}
		}
	}
	return b.String(), images
}

// ContentValue is a message's content for an OpenAI body: the text alone, as it
// always was, or — when the message carries images — content parts, the text
// first and then one `image_url` part per image.
func ContentValue(text string, images []string) any {
	if len(images) == 0 {
		return text
	}
	parts := make([]map[string]any, 0, len(images)+1)
	if text != "" {
		parts = append(parts, map[string]any{"type": "text", "text": text})
	}
	for _, u := range images {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": u}})
	}
	return parts
}

// StripDataURL is an image as Ollama takes it: the bare base64 payload of a
// data URL; any other string is returned as given.
func StripDataURL(s string) string {
	if !strings.HasPrefix(s, "data:") {
		return s
	}
	if i := strings.Index(s, ","); i >= 0 {
		return s[i+1:]
	}
	return s
}

// HasImages reports whether any message carries an image.
func HasImages(msgs []Message) bool {
	for _, m := range msgs {
		if len(m.Images) > 0 {
			return true
		}
	}
	return false
}
