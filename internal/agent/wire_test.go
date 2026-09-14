package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

// A ProcessSpec travels leader → worker as JSON. A field that cannot be
// encoded (the Adapt hook is a func) must be excluded, not fail the encode:
// on 2026-09-14 a leader posted an empty body to every gang part because of it.
func TestProcessSpecIsWireSafe(t *testing.T) {
	spec := ProcessSpec{ID: "s", Command: "rpc-server", Args: []string{"-p", "50052"}, Adapt: func([]string) ([]string, bool) { return nil, false }}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("ProcessSpec must encode: %v", err)
	}
	var back ProcessSpec
	if err := json.Unmarshal(raw, &back); err != nil || back.ID != "s" || back.Command != "rpc-server" || len(back.Args) != 2 {
		t.Fatalf("round trip: %v %+v", err, back)
	}
	if strings.Contains(string(raw), "dapt") {
		t.Fatalf("the hook must not be on the wire: %s", raw)
	}
}
