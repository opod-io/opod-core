package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// The router could not reach the worker it picked and had no other to ask. The
// client got 502 "llamacpp at http://127.0.0.1:8089 is not reachable … Start
// llama.cpp: llama-server -m …" — the LEADER's own engine and a hint to start
// it, for a fault that is a worker that went away (parked, moved, died). That
// is "nothing can take this right now": 503 + Retry-After, in the worker's
// name. Seen on a cluster as a parked endpoint's first answer.
func TestAnUnreachableWorkerIsNoCapacityNotALocalEngineFault(t *testing.T) {
	gone := engines.Unreachable("vllm", "http://10.0.0.7:8081", errors.New("dial tcp: i/o timeout"))
	msg, ok := workerGone("n_pod-abc", gone)
	if !ok || !strings.Contains(msg, "n_pod-abc") || strings.Contains(msg, "llama-server") {
		t.Fatalf("an unreachable WORKER: want a no-capacity message naming it, got %q %v", msg, ok)
	}
	for name, tc := range map[string]struct {
		node string
		err  error
	}{
		"the leader's own engine keeps its start hint": {"local", gone},
		"no worker was ever reached":                   {"", gone},
		"a shard coordinator has its own message":      {"shard:m", gone},
		"an engine's answer is not a missing worker":   {"n_pod-abc", engines.Upstream("vllm", "chat", 500, nil)},
		"any other error":                              {"n_pod-abc", errors.New("boom")},
	} {
		if _, ok := workerGone(tc.node, tc.err); ok {
			t.Errorf("%s: must not be reported as a gone worker", name)
		}
	}
}
