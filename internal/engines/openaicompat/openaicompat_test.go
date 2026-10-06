package openaicompat

import (
	"encoding/json"
	"strings"
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

// Tool declarations go down verbatim, and the model's call fragments come
// back up the same way — the merge belongs to whoever needs one answer, not
// to the stream reader (T10.8).
func TestToolsDownAndToolCallsUp(t *testing.T) {
	tools := `[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]`
	body := BuildChatBody(engines.ChatRequest{
		Model:      "m",
		Tools:      json.RawMessage(tools),
		ToolChoice: json.RawMessage(`"auto"`),
	})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var sent struct {
		Tools      json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if string(sent.Tools) != tools {
		t.Fatalf("tools changed in flight:\n got %s\nwant %s", sent.Tools, tools)
	}
	if string(sent.ToolChoice) != `"auto"` {
		t.Fatalf("tool_choice = %s", sent.ToolChoice)
	}
	if _, present := BuildChatBody(engines.ChatRequest{Model: "m"})["tools"]; present {
		t.Fatal("tools is on the body when the caller declared none")
	}
}

// The loop's second turn on the wire: the assistant's calls and the result's
// id must both reach the engine, or a correct request is refused as if the
// caller had made a mistake.
func TestBuildChatBodyCarriesTheToolLoop(t *testing.T) {
	body := BuildChatBody(engines.ChatRequest{
		Model: "m",
		Messages: []engines.Message{
			{Role: "user", Content: "weather?"},
			{Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]`)},
			{Role: "tool", ToolCallID: "call_1", Content: "sunny"},
		},
	})
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var sent struct {
		Messages []struct {
			Role       string          `json:"role"`
			ToolCalls  json.RawMessage `json:"tool_calls"`
			ToolCallID string          `json:"tool_call_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(sent.Messages) != 3 {
		t.Fatalf("got %d messages", len(sent.Messages))
	}
	if !strings.Contains(string(sent.Messages[1].ToolCalls), `"call_1"`) {
		t.Fatalf("assistant tool_calls lost: %q", sent.Messages[1].ToolCalls)
	}
	if sent.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("tool_call_id lost: %+v", sent.Messages[2])
	}
	// A plain turn carries neither key.
	plain, _ := json.Marshal(BuildChatBody(engines.ChatRequest{
		Model: "m", Messages: []engines.Message{{Role: "user", Content: "hi"}},
	}))
	if strings.Contains(string(plain), "tool_call") {
		t.Fatalf("a plain message grew a tool key: %s", plain)
	}
}
