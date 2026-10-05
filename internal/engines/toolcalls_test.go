package engines

import (
	"encoding/json"
	"testing"
)

// A model writes one tool call across many chunks: the first names it, the
// rest carry slices of the argument string. Only the index is on every
// fragment, so the merge is keyed by it (T10.8).
func TestToolCallAccumulatorMergesFragments(t *testing.T) {
	var a ToolCallAccumulator
	if !a.Empty() {
		t.Fatal("a fresh accumulator is not empty")
	}
	for _, frag := range []string{
		`[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]`,
		`[{"index":0,"function":{"arguments":"{\"city\""}}]`,
		`[{"index":0,"function":{"arguments":":\"Oslo\"}"}}]`,
	} {
		a.Add(json.RawMessage(frag))
	}
	calls := a.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want 1", len(calls))
	}
	if calls[0]["id"] != "call_1" || calls[0]["type"] != "function" {
		t.Fatalf("identity lost: %v", calls[0])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("name = %v", fn["name"])
	}
	// arguments stays a STRING on the wire — the caller parses it, because
	// only they know the schema they asked for.
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments is %T, want string", fn["arguments"])
	}
	if args != `{"city":"Oslo"}` {
		t.Fatalf("arguments = %q", args)
	}
}

// Two calls in one answer keep their own fragments and the order they first
// appeared in, even when their fragments interleave.
func TestToolCallAccumulatorKeepsCallsApart(t *testing.T) {
	var a ToolCallAccumulator
	a.Add(json.RawMessage(`[{"index":1,"id":"b","function":{"name":"second","arguments":"{"}}]`))
	a.Add(json.RawMessage(`[{"index":0,"id":"a","function":{"name":"first","arguments":"{"}}]`))
	a.Add(json.RawMessage(`[{"index":1,"function":{"arguments":"}"}},{"index":0,"function":{"arguments":"}"}}]`))

	calls := a.Calls()
	if len(calls) != 2 {
		t.Fatalf("got %d calls, want 2", len(calls))
	}
	if calls[0]["id"] != "b" || calls[1]["id"] != "a" {
		t.Fatalf("order is not first-appearance: %v", calls)
	}
	for _, c := range calls {
		if got := c["function"].(map[string]any)["arguments"]; got != "{}" {
			t.Fatalf("arguments merged across calls: %v", calls)
		}
	}
}

// A fragment with no index is the only call there is, a missing type is the
// one type the API has, and a payload that does not parse is ignored rather
// than guessed at.
func TestToolCallAccumulatorEdges(t *testing.T) {
	var a ToolCallAccumulator
	a.Add(nil)
	a.Add(json.RawMessage(`not json`))
	a.Add(json.RawMessage(`[]`))
	if !a.Empty() || a.Calls() != nil {
		t.Fatalf("nothing usable arrived, yet Calls() = %v", a.Calls())
	}

	var b ToolCallAccumulator
	b.Add(json.RawMessage(`[{"function":{"name":"only","arguments":"{}"}}]`))
	calls := b.Calls()
	if len(calls) != 1 || calls[0]["type"] != "function" || calls[0]["index"] != 0 {
		t.Fatalf("defaults wrong: %v", calls)
	}
	if _, hasID := calls[0]["id"]; hasID {
		t.Fatal("an id was invented for a fragment that carried none")
	}
}
