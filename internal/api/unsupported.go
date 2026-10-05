package api

import (
	"encoding/json"
	"net/http"
)

// Parts of the OpenAI chat schema this gateway parses and cannot honour.
//
// Go's decoder discards unknown fields, so before this file a request
// carrying `tools` was answered as if it had never carried one: the engine
// never saw the tools, the model never emitted a tool call, and the caller
// got a paragraph of prose where their agent expected a function call —
// with nothing in any log saying why. A silent wrong answer is the one
// failure shape this product refuses everywhere else by name (E-FIT,
// E-KV-SIGNAL, E-PD-ENGINE), so it refuses here too (PLAN T10.13).
//
// This is deliberately a REFUSAL and not an implementation. Carrying `tools`
// down to the engine and `tool_calls` back up is T10.8; honouring
// `response_format` is T10.14. Each one narrows this check rather than
// deleting it: what is left afterwards is "this ENGINE cannot", which is the
// shape every other capability refusal already has.
//
// Executing a tool is never ours. We would carry the field and the call; the
// caller runs the function. That line is the same one ADR-077 §5 draws for
// accounts and quotas.
// Only the CURRENT OpenAI shape is read. The deprecated `functions` /
// `function_call` pair is not handled anywhere in this runtime and is not
// handled here either: nothing of ours is in production (ADR-078), so there
// is no caller to keep compatible, and a refusal for a field we would never
// serve is one more thing to carry.
type unsupportedChatFields struct {
	Tools      []json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage   `json:"tool_choice"`

	ResponseFormat *struct {
		Type string `json:"type"`
	} `json:"response_format"`
}

// refuseUnsupportedChatFields answers 400 and names the field when the body
// asks for something the gateway cannot do, and reports whether it did. A
// body that does not parse is left alone: the handler's own Unmarshal
// produces the invalid-JSON error, so there is one place that says that.
//
// What it does NOT refuse, because the request is then answered correctly:
//   - `tool_choice: "none"` — the caller is saying "do not call a tool", and
//     plain text is the right answer even with tools declared.
//   - `response_format: {"type": "text"}` — that is what we return anyway.
//     Only json_object / json_schema are refused.
func refuseUnsupportedChatFields(w http.ResponseWriter, body []byte) bool {
	var f unsupportedChatFields
	if json.Unmarshal(body, &f) != nil {
		return false
	}
	if len(f.Tools) > 0 && !isNoneChoice(f.ToolChoice) {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"`tools` is not supported by this gateway: the tools would be dropped before the model saw them "+
				"and you would get plain text back. Remove `tools`, or send `\"tool_choice\": \"none\"` if a text "+
				"answer is what you want.")
		return true
	}
	if f.ResponseFormat != nil && f.ResponseFormat.Type != "" && f.ResponseFormat.Type != "text" {
		writeJSONError(w, http.StatusBadRequest, "invalid_request",
			"`response_format: "+f.ResponseFormat.Type+"` is not supported by this gateway: the request would be "+
				"answered as free text with no guarantee it parses. Remove `response_format`, or send "+
				"`{\"type\": \"text\"}`.")
		return true
	}
	return false
}

// isNoneChoice reports whether a tool_choice value is the string "none" —
// the one value that makes a text answer correct.
func isNoneChoice(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == "none"
}
