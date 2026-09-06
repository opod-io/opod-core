package controlplane

// model="auto" resolves to this leader's default model. The vendor routing
// chain (and its egress) left core with ADR-022; catalog fallback chains for
// local models live in the router (fallback_run.go).

import (
	"encoding/json"
	"strings"
)

func isAutoModel(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), "auto")
}

// setModel rewrites the JSON body's "model" field to id (used to retarget a
// request at each chain candidate). Returns the body unchanged if it isn't
// valid JSON.
func setModel(body []byte, id string) []byte {
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["model"] = id
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}
