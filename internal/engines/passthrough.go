package engines

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// The OpenAI chat body, passed through (PLAN T17.1).
//
// The gateway decodes a chat request into a struct that names the fields opod
// reads, and json.Unmarshal silently discards every other key. Re-serialising
// only what the struct names dropped `seed`, `logprobs`, `top_k`, `min_p`,
// `presence_penalty`, `max_completion_tokens`, `reasoning_effort` and whatever
// the next OpenAI revision adds — so each engine feature had to be its own
// change. Instead, every top-level key opod does not read rides along as raw
// JSON (ChatRequest.Extra) and is merged into the engine body last.
//
// It is a deny-list, not a blind merge: the keys below are opod's, and a caller
// who could set them would break the request path. `model` is the endpoint's one
// model identity (D5); `messages` is what the guardrails inspected; `stream` is
// always true towards the engine (the gateway aggregates when the caller asked
// for one answer); and `stream_options` carries `include_usage`, so a caller
// who turned it off would silently break usage capture.

// reservedChatKeys are the body keys opod owns on the way to an engine. A
// caller's value for one of them is never forwarded as an extra.
var reservedChatKeys = map[string]bool{
	"model":          true,
	"messages":       true,
	"stream":         true,
	"stream_options": true,
}

// IsReservedChatKey reports whether key belongs to opod rather than the caller.
func IsReservedChatKey(key string) bool { return reservedChatKeys[key] }

// JSONFieldNames returns the top-level JSON key of every exported field of the
// struct v (or *v): the keys a decode into v already consumes. Computed once
// per request type, at package init, never per request.
func JSONFieldNames(v any) map[string]bool {
	t := reflect.TypeOf(v)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name := f.Name
		if tag, ok := f.Tag.Lookup("json"); ok {
			if tag == "-" {
				continue
			}
			if n, _, _ := strings.Cut(tag, ","); n != "" {
				name = n
			}
		}
		out[name] = true
	}
	return out
}

// ExtraFields returns the top-level keys of the JSON object body that named
// does not hold and that are not reserved, each as the caller's raw JSON. nil
// when there are none, which is the common request.
func ExtraFields(body []byte, named map[string]bool) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, err
	}
	var out map[string]json.RawMessage
	for k, v := range all {
		if named[k] || reservedChatKeys[k] {
			continue
		}
		if out == nil {
			out = make(map[string]json.RawMessage, len(all))
		}
		out[k] = v
	}
	return out, nil
}

// CheckSingleChoice refuses the extras that ask for more than one answer.
//
// The stream decoder reads `choices[0]` and nothing else, so a request for four
// completions would be generated (and billed) four times and answered once. An
// `n` of 1 and a `best_of` of 1 ask for nothing extra and pass; a value that is
// not a number is left for the engine to refuse in its own words.
func CheckSingleChoice(extra map[string]json.RawMessage) error {
	if n, ok := jsonNumber(extra["n"]); ok && n != 1 {
		return fmt.Errorf("`n` = %v is not supported: this endpoint returns exactly one choice per request; send %v requests instead", n, n)
	}
	if b, ok := jsonNumber(extra["best_of"]); ok && b > 1 {
		return fmt.Errorf("`best_of` = %v is not supported: this endpoint generates and returns exactly one choice per request", b)
	}
	return nil
}

// jsonNumber reads raw as a number; false when it is absent, null or not one.
func jsonNumber(raw json.RawMessage) (float64, bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, false
	}
	return f, true
}

// LogprobsAccumulator merges the per-chunk `choices[0].logprobs` objects of a
// streamed answer into the one object a non-streamed answer carries. OpenAI's
// chat shape is {"content":[{token, logprob, bytes, top_logprobs}, …]}: each
// chunk holds the entries for its own tokens, so the merge is a concatenation,
// in arrival order. The zero value is ready to use.
//
// Without it a request that asked for logprobs reached the engine (T17.1) and
// the answer it produced was dropped on the way back — the request half of a
// passthrough with no response half.
type LogprobsAccumulator struct {
	content []json.RawMessage
	seen    bool
}

// Add folds one chunk's raw logprobs object. A payload that does not parse is
// ignored: the tokens it carried are lost, the answer is not.
func (a *LogprobsAccumulator) Add(raw json.RawMessage) {
	if !HasJSON(raw) {
		return
	}
	var lp struct {
		Content []json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &lp); err != nil {
		return
	}
	a.seen = true
	a.content = append(a.content, lp.Content...)
}

// JSON is the merged object, or nil when no chunk carried one — the key is
// then absent from the answer rather than invented.
func (a *LogprobsAccumulator) JSON() json.RawMessage {
	if !a.seen {
		return nil
	}
	content := a.content
	if content == nil {
		content = []json.RawMessage{}
	}
	b, err := json.Marshal(map[string]any{"content": content})
	if err != nil {
		return nil
	}
	return b
}
