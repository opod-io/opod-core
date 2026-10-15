package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// Both engines of a model were killed on the design-partner cell and two
// requests the gate had let through answered 502 "llamacpp at
// http://127.0.0.1:8089 is not reachable … Start llama.cpp: llama-server …":
// the heartbeat dropped the placements between the gate and the pick, the
// router fell back to the leader's own engine, and a leader that serves
// through workers has none (PLAN T8.21). Asked again, the gate names why.
func TestNoWorkerLeftByThePickAsksTheGateAgain(t *testing.T) {
	refused := engines.Unreachable("llamacpp", "http://127.0.0.1:8089", errors.New("dial tcp 127.0.0.1:8089: connect: connection refused"))
	const why = "no worker can take a new request for m right now: 2 worker(s) whose engine is restarting — new requests resume when it serves again; retry shortly"
	h := &Handler{Unavailable: func(_ context.Context, model string) string {
		if model != "m" {
			t.Fatalf("asked about %q", model)
		}
		return why
	}}
	for _, node := range []string{"local", ""} {
		got, ok := h.capacityGone(context.Background(), "m", node, refused)
		if !ok || got != why || strings.Contains(got, "llama-server") {
			t.Fatalf("node %q: want the gate's reason, got %q %v", node, got, ok)
		}
	}

	// The gate says something can serve: the leader's own engine really
	// failed, and keeps its own description (classifyEngineError).
	serving := &Handler{Unavailable: func(context.Context, string) string { return "" }}
	if _, ok := serving.capacityGone(context.Background(), "m", "local", refused); ok {
		t.Fatal("a servable model's local engine fault is not no capacity")
	}
	// A standalone handler with no gate keeps today's answer.
	if _, ok := (&Handler{}).capacityGone(context.Background(), "m", "local", refused); ok {
		t.Fatal("no gate: no second opinion")
	}
	// A worker's failure has its own answer (workerGone); an engine's own
	// non-2xx answer is not "nothing answered".
	if _, ok := h.capacityGone(context.Background(), "m", "n_pod-abc", refused); ok {
		t.Fatal("a named worker is workerGone's, not this")
	}
	if _, ok := h.capacityGone(context.Background(), "m", "local", engines.Upstream("llamacpp", "chat", 500, nil)); ok {
		t.Fatal("an engine that answered is not a missing worker")
	}
}
