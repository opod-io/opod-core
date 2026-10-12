package api

// A worker's engine that is down reached the caller as 502 "llamacpp at
// http://127.0.0.1:8089 is not reachable … Start llama.cpp: llama-server -m …"
// — the LEADER's own engine and a hint to start it, for a fault on a worker
// (design-partner cell, PLAN T8.18).

import (
	"errors"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

func TestAWorkersDeadEngineIsNoCapacityInTheWorkersName(t *testing.T) {
	err := engines.FromWorkerAnswer("vllm", "http://10.0.0.7:8081", "chat", 502,
		[]byte(`{"error":{"type":"engine_unreachable_on_worker","message":"engine: dial tcp 127.0.0.1:8089: connect: connection refused"}}`))
	msg, ok := workerGone("n_pod-abc", err)
	if !ok || !strings.Contains(msg, "engine on worker n_pod-abc") || strings.Contains(msg, "llama-server") {
		t.Fatalf("want no capacity in the worker's name, got %q %v", msg, ok)
	}
}

// An older worker's untyped 502 is still the engine's error, but it came from
// a server that answered: never the leader's own engine and its start hint.
func TestAnEngineErrorFromAWorkerCarriesNoLocalStartHint(t *testing.T) {
	eng := engines.MustNew("llamacpp", "http://127.0.0.1:8089", "")
	err := engines.Upstream("vllm", "chat", 502,
		[]byte("engine: llamacpp unreachable at http://127.0.0.1:8089: dial tcp 127.0.0.1:8089: connect: connection refused"))
	code, msg := classifyEngineError(eng, err)
	if code != "upstream_error" || strings.Contains(msg, "llama-server") {
		t.Fatalf("a worker's answer is not the leader's engine down: %s %q", code, msg)
	}
	// The leader's own engine, unreachable, keeps its hint.
	code, msg = classifyEngineError(eng, engines.Unreachable("llamacpp", "http://127.0.0.1:8089", errors.New("connect: connection refused")))
	if code != "engine_unreachable" || !strings.Contains(msg, "llama-server") {
		t.Fatalf("the leader's own engine down keeps its start hint: %s %q", code, msg)
	}
}
