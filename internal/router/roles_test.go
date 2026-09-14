package router

import (
	"testing"

	"github.com/opod-io/opod/internal/store"
)

func TestDecodeOnlyKeepsCompleteServersAndDecodeHalves(t *testing.T) {
	roles := map[string]string{"p": "prefill", "d": "decode", "c": ""}
	role := func(id string) string { return roles[id] }
	ws := []store.Placement{{NodeID: "p"}, {NodeID: "d"}, {NodeID: "c"}}
	got := decodeOnly(ws, role)
	if len(got) != 2 || got[0].NodeID != "d" || got[1].NodeID != "c" {
		t.Fatalf("prefill dropped when a decode half exists: %v", got)
	}
	// No decode half anywhere → nothing filtered (a misconfigured pair still serves).
	only := []store.Placement{{NodeID: "p"}, {NodeID: "c"}}
	if got := decodeOnly(only, role); len(got) != 2 {
		t.Fatalf("no decode → unchanged: %v", got)
	}
	if roleOf(&store.Node{HardwareJSON: `{"Hostname":"x","Role":"decode"}`}) != "decode" || roleOf(&store.Node{HardwareJSON: `{"Hostname":"x"}`}) != "" || roleOf(nil) != "" {
		t.Fatal("roleOf reads the capabilities' Role")
	}
}
