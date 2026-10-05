package openaicompat

import (
	"encoding/json"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// A caller's response_format reaches the engine verbatim: the schema is the
// caller's own JSON and a re-encode can only lose what we do not model yet
// (T10.14).
func TestBuildChatBodyCarriesResponseFormat(t *testing.T) {
	schema := `{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object"}}}`
	body := BuildChatBody(engines.ChatRequest{
		Model:          "m",
		Messages:       []engines.Message{{Role: "user", Content: "hi"}},
		ResponseFormat: json.RawMessage(schema),
	})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		ResponseFormat json.RawMessage `json:"response_format"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(got.ResponseFormat) != schema {
		t.Fatalf("response_format changed in flight:\n got %s\nwant %s", got.ResponseFormat, schema)
	}

	// Asking for nothing sends nothing — no empty key on the wire.
	plain := BuildChatBody(engines.ChatRequest{Model: "m"})
	if _, present := plain["response_format"]; present {
		t.Fatal("response_format is on the body when the caller sent none")
	}
}
