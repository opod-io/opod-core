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
		"a shard coordinator has its own message":      {"shard:m", gone}, // gangGone, asserted below
		"an engine's answer is not a missing worker":   {"n_pod-abc", engines.Upstream("vllm", "chat", 500, nil)},
		"any other error":                              {"n_pod-abc", errors.New("boom")},
	} {
		if _, ok := workerGone(tc.node, tc.err); ok {
			t.Errorf("%s: must not be reported as a gone worker", name)
		}
	}
}

// …and that own message has to exist. The exclusion above was written in the
// belief that a gang had its own answer, and it did not: the dispatch fell
// through to classifyEngineError and the caller was told the LEADER's engine
// was unreachable, with a hint to start llama-server, while the real state was
// a gang whose coordinator had just been formed and whose vLLM was still
// loading (measured on the design-partner cell, 2026-09-20, in the window
// right after a park/wake).
func TestAGangStillFormingIsNoCapacityNotALocalEngineFault(t *testing.T) {
	gone := engines.Unreachable("vllm", "http://10.42.1.72:9100", errors.New("connect: connection refused"))
	msg, ok := gangGone("shard:qwen2.5-1.5b-instruct:g0", gone)
	if !ok {
		t.Fatal("an unreachable gang coordinator is no capacity right now, not a local engine fault")
	}
	if strings.Contains(msg, "llama-server") || strings.Contains(msg, "127.0.0.1") {
		t.Errorf("the message must not describe the leader's own engine: %q", msg)
	}
	if !strings.Contains(msg, "retry") {
		t.Errorf("a wake in progress is something the caller can act on: %q", msg)
	}
	for name, tc := range map[string]struct {
		node string
		err  error
	}{
		"a plain worker belongs to workerGone": {"n_pod-abc", gone},
		"the leader's own engine":              {"local", gone},
		"nothing was reached":                  {"", gone},
		"an engine's own answer is not this":   {"shard:m:g0", engines.Upstream("vllm", "chat", 500, nil)},
	} {
		if _, ok := gangGone(tc.node, tc.err); ok {
			t.Errorf("%s: must not be reported as a gang that cannot be reached", name)
		}
	}
}
