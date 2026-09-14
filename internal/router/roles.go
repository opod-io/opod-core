package router

// Prefill/decode roles (feature pd_roles, R9.7): a worker registers a role in
// its capabilities; generation goes to decode workers when a pair is present.
// A prefill half is not a complete server — routing a chat to it would
// produce the first token and nothing after — so it never receives a
// generation request while a decode half of the same model is alive. With no
// decode worker at all (roles misconfigured, or a plan of complete servers)
// nothing is filtered: the old behaviour, exactly. The KV-cache handoff that
// makes the pair faster than one server is TARGET; roles are recorded and
// respected ahead of it so a plan can state them today.

import (
	"encoding/json"

	"github.com/opod-io/opod/internal/store"
)

// roleOf reads the role a worker registered (capabilities JSON, field Role).
func roleOf(n *store.Node) string {
	if n == nil || n.HardwareJSON == "" {
		return ""
	}
	var caps struct {
		Role string `json:"Role"`
	}
	if json.Unmarshal([]byte(n.HardwareJSON), &caps) != nil {
		return ""
	}
	return caps.Role
}

// decodeOnly drops prefill halves from the candidates when a decode half is
// among them; roles come from the node rows the caller supplies.
func decodeOnly(workers []store.Placement, role func(nodeID string) string) []store.Placement {
	hasDecode := false
	for _, w := range workers {
		if role(w.NodeID) == "decode" {
			hasDecode = true
			break
		}
	}
	if !hasDecode {
		return workers
	}
	out := workers[:0:0]
	for _, w := range workers {
		if role(w.NodeID) != "prefill" {
			out = append(out, w)
		}
	}
	return out
}
