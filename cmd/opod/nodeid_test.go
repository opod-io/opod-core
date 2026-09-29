package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A worker's identity precedence (PLAN T15.15, G19): an explicit id, then the
// persisted one, then the pod's name, and only with none of those a random
// id — so a worker in a pod with no volume and no pinned id still keeps its
// identity across restarts.
func TestNodeIDPrecedence(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "node.yaml")
	if err := os.WriteFile(persisted, []byte("node_id: n_persisted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := nodeIDFrom("n_explicit", readPersistedNodeID(persisted), "worker-0"); got != "n_explicit" {
		t.Fatalf("explicit wins: %q", got)
	}
	if got := nodeIDFrom("", readPersistedNodeID(persisted), "worker-0"); got != "n_persisted" {
		t.Fatalf("persisted beats the pod name: %q", got)
	}
	if got := nodeIDFrom("", readPersistedNodeID(filepath.Join(dir, "absent.yaml")), "chat-w-abc12"); got != "n_chat-w-abc12" {
		t.Fatalf("the pod name beats a random id: %q", got)
	}
	a, b := nodeIDFrom("", "", ""), nodeIDFrom("", "", "")
	if !strings.HasPrefix(a, "n_") || a == b {
		t.Fatalf("with nothing to go on the id is random: %q %q", a, b)
	}
}
