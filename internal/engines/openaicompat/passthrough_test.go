package openaicompat

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// Every field the caller sent and we do not model reaches the engine body
// verbatim (T17.1): an engine feature arrives without a row of its own.
func TestBuildChatBodyCarriesTheCallersExtraFields(t *testing.T) {
	body := BuildChatBody(engines.ChatRequest{
		Model:    "m",
		Messages: []engines.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
		Extra: map[string]json.RawMessage{
			"seed":                json.RawMessage(`42`),
			"logprobs":            json.RawMessage(`true`),
			"top_logprobs":        json.RawMessage(`3`),
			"a_field_from_2027":   json.RawMessage(`{"nested":[1,2]}`),
			"max_completion_tokn": json.RawMessage(`"typo stays the caller's"`),
		},
	})
	raw, _ := json.Marshal(body)
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"seed": `42`, "logprobs": `true`, "top_logprobs": `3`,
		"a_field_from_2027": `{"nested":[1,2]}`, "max_completion_tokn": `"typo stays the caller's"`,
	} {
		if string(got[k]) != want {
			t.Errorf("%s = %s, want %s", k, got[k], want)
		}
	}
}

// The reserved keys are ours even when an extra names one: a slip upstream
// must not let a caller switch off usage capture, stop the engine streaming,
// or change the model this endpoint is (D5).
func TestAnExtraCannotOverrideAReservedKey(t *testing.T) {
	body := BuildChatBody(engines.ChatRequest{
		Model:    "ours",
		Messages: []engines.Message{{Role: "user", Content: "hi"}},
		Stream:   true,
		Extra: map[string]json.RawMessage{
			"model":          json.RawMessage(`"theirs"`),
			"stream":         json.RawMessage(`false`),
			"stream_options": json.RawMessage(`{"include_usage":false}`),
			"messages":       json.RawMessage(`[]`),
		},
	})
	raw, _ := json.Marshal(body)
	var got struct {
		Model         string            `json:"model"`
		Stream        bool              `json:"stream"`
		StreamOptions map[string]bool   `json:"stream_options"`
		Messages      []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "ours" || !got.Stream || !got.StreamOptions["include_usage"] || len(got.Messages) != 1 {
		t.Fatalf("a reserved key was overridden by an extra: %s", raw)
	}
}

// Logprobs come back up on the event of the content they describe, so the
// answer carries what the request asked for (T17.1).
func TestConsumeStreamCarriesLogprobs(t *testing.T) {
	sse := `data: {"choices":[{"delta":{"content":"Hi"},"logprobs":{"content":[{"token":"Hi","logprob":-0.1}]}}]}

data: {"choices":[{"delta":{"content":"!"},"logprobs":null}]}

data: {"choices":[{"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`
	evs := collect(context.Background(), sse, false)
	if len(evs) < 3 {
		t.Fatalf("events: %+v", evs)
	}
	if evs[0].Delta != "Hi" || string(evs[0].Logprobs) != `{"content":[{"token":"Hi","logprob":-0.1}]}` {
		t.Fatalf("first event lost its logprobs: %+v", evs[0])
	}
	if evs[1].Delta != "!" || evs[1].Logprobs != nil {
		t.Fatalf("a null logprobs became something: %+v", evs[1])
	}
}
