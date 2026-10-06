package engines

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// Merging a model's tool call back into one object.
//
// A tool call arrives in fragments. The first chunk of a call carries its
// index, its id, its type and the function name; every chunk after it carries
// another slice of the argument STRING, which is a JSON document the model is
// writing one piece at a time. Only the index is guaranteed on every
// fragment, and it is what ties them together — ids and names appear once.
//
// A streaming caller gets the fragments as they come and does this merge
// themselves, because that is what the OpenAI wire says. A caller who asked
// for one answer cannot, so the aggregating side does it here — once, shared
// by the leader's /v1 handler and the worker's, which would otherwise grow
// two copies of the same fiddly loop (PLAN T10.8).

// ToolCallAccumulator merges `delta.tool_calls` fragments into whole calls.
// The zero value is ready to use.
type ToolCallAccumulator struct {
	order   []int                  // indexes, in the order they first appeared
	calls   map[int]*toolCallBuild // by the fragment's own index
	skipped int                    // fragments that did not parse; see Skipped
}

// Skipped is how many fragments were dropped because they did not parse. A
// caller logs it rather than guessing: a non-zero count beside a non-empty
// answer means the merge is INCOMPLETE, which is worth a line in the log and
// is invisible otherwise.
func (a *ToolCallAccumulator) Skipped() int { return a.skipped }

type toolCallBuild struct {
	ID   string
	Type string
	Name string
	Args []byte // the argument string, concatenated
}

// fragment is one entry of a `delta.tool_calls` array. Every field but Index
// is optional, which is the whole reason this type exists.
type fragment struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// Add folds one event's raw `delta.tool_calls` array into the accumulator.
// A payload that does not parse is ignored rather than guessed at: a
// half-understood tool call is worse than none, and the stream carries the
// engine's own errors separately.
func (a *ToolCallAccumulator) Add(raw json.RawMessage) {
	if !HasJSON(raw) {
		return
	}
	// Per ENTRY, not per array: one fragment an engine shapes differently
	// (arguments as an object, say) used to discard every call in the chunk,
	// and the caller then got `finish_reason: "tool_calls"` with no calls —
	// the exact shape this feature exists to end. A bad entry is skipped; its
	// siblings are kept (review finding, 2026-10-05).
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return
	}
	frags := make([]fragment, 0, len(entries))
	for _, e := range entries {
		var f fragment
		if json.Unmarshal(e, &f) != nil {
			a.skipped++
			continue
		}
		frags = append(frags, f)
	}
	for _, f := range frags {
		idx := 0
		if f.Index != nil {
			idx = *f.Index
		}
		if a.calls == nil {
			a.calls = map[int]*toolCallBuild{}
		}
		c, seen := a.calls[idx]
		if !seen {
			c = &toolCallBuild{}
			a.calls[idx] = c
			a.order = append(a.order, idx)
		}
		if f.ID != "" {
			c.ID = f.ID
		}
		if f.Type != "" {
			c.Type = f.Type
		}
		if f.Function != nil {
			if f.Function.Name != "" {
				c.Name = f.Function.Name
			}
			c.Args = append(c.Args, f.Function.Arguments...)
		}
	}
}

// Empty reports whether no fragment ever arrived, which is every ordinary
// answer.
func (a *ToolCallAccumulator) Empty() bool { return len(a.order) == 0 }

// Calls renders the merged calls in the OpenAI response shape, in the order
// their indexes first appeared. `arguments` stays a STRING, as the wire
// defines it — the caller parses it, because only they know the schema they
// asked for. A call whose fragments never named a type is given "function",
// the only type the API has.
func (a *ToolCallAccumulator) Calls() []map[string]any {
	if a.Empty() {
		return nil
	}
	out := make([]map[string]any, 0, len(a.order))
	for n, idx := range a.order {
		c := a.calls[idx]
		typ := c.Type
		if typ == "" {
			typ = "function"
		}
		// `id` is REQUIRED on a response's tool call — the caller has to put
		// it in `tool_call_id` on the result message, and the SDKs validate
		// it — so one is synthesised when the engine sent none. `index`, by
		// contrast, is a streaming-delta field and has no place here; it is
		// the merge key, not part of the answer (review finding, 2026-10-05).
		id := c.ID
		if id == "" {
			id = "call_" + strconv.Itoa(n)
		}
		out = append(out, map[string]any{
			"id":   id,
			"type": typ,
			"function": map[string]any{
				"name":      c.Name,
				"arguments": string(c.Args),
			},
		})
	}
	return out
}

// HasJSON reports whether a raw field carries something worth sending on.
//
// `json.RawMessage` keeps the bytes it was given, so a client that serialises
// an unset field rather than omitting it hands us the four bytes `null` — and
// a bare length check then treats `"tools": null` as a tool declaration. Some
// clients and proxies do exactly that, so every test of "did the caller ask
// for this?" goes through here rather than through len() (review finding,
// 2026-10-05).
func HasJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	switch {
	case len(t) == 0:
		return false
	case bytes.Equal(t, []byte("null")), bytes.Equal(t, []byte("[]")):
		return false
	}
	return true
}
