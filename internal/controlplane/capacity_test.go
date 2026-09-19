package controlplane

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

const soloChatBody = `{"model":"solo","messages":[{"role":"user","content":"hi"}]}`

// The capacity question is per model. Two models on one leader: "m" on both
// workers, "solo" on w1 alone. When w1 is drained — or lost — "solo" answers
// the 503 + Retry-After every other "not right now" gets, saying which it is,
// while "m" keeps serving; it used to pass the leader-wide check, fall to the
// local engine and answer 502.
func TestCapacityIsAskedPerModel(t *testing.T) {
	srv, ts, workers := drainFixture(t) // w1 and w2 both serve "m"; the leader's own engine is down
	ctx := context.Background()
	admin := Caller{Admin: true}
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w1", LoadedModels: []string{"m", "solo"}}, admin); err != nil {
		t.Fatal(err)
	}
	if resp, out := chat(t, ts, soloChatBody, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("solo while w1 serves: %d %s", resp.StatusCode, out)
	}

	// Drained.
	if err := srv.DrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	resp, out := chat(t, ts, soloChatBody, "")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("solo with its only worker drained: %d (Retry-After %q) %s, want 503 + Retry-After", resp.StatusCode, resp.Header.Get("Retry-After"), out)
	}
	if !strings.Contains(out, "draining") || !strings.Contains(out, "undrain") || strings.Contains(out, "scale-up") {
		t.Errorf("the 503 must say it is a drain, not a scale-up: %s", out)
	}
	before := workers["w2"].chats.Load()
	if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("m must keep serving while solo cannot: %d %s", resp.StatusCode, out)
	}
	if workers["w2"].chats.Load() != before+1 {
		t.Error("m was not served by the worker that is left")
	}

	// Lost.
	if err := srv.UndrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.Nodes().Heartbeat(ctx, "w1", time.Now().Add(-10*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	resp, out = chat(t, ts, soloChatBody, "")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || !strings.Contains(out, "stopped heartbeating") {
		t.Fatalf("solo with its only worker lost: %d %s", resp.StatusCode, out)
	}

	// Asleep: the waking text, unchanged.
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w1", LoadedModels: []string{"m", "solo"}, Sleeping: true}, admin); err != nil {
		t.Fatal(err)
	}
	resp, out = chat(t, ts, soloChatBody, "")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(out, "waking") {
		t.Fatalf("solo with its only worker asleep: %d %s", resp.StatusCode, out)
	}

	// Back: served again, nothing sticks.
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w1", LoadedModels: []string{"m", "solo"}}, admin); err != nil {
		t.Fatal(err)
	}
	if resp, out := chat(t, ts, soloChatBody, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("solo after w1 is back: %d %s", resp.StatusCode, out)
	}
}

// Nothing holds the model and nothing serves at all: the waking answer, word
// for word what it was (a scale-from-zero endpoint depends on it).
func TestNothingHeldIsStillWaking(t *testing.T) {
	srv, ts, _ := drainFixture(t)
	for _, id := range []string{"w1", "w2"} {
		if err := srv.RemoveNode(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	resp, out := chat(t, ts, drainChatBody, "")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" || !strings.Contains(out, "no workers are awake for this model — waking (scale-up in progress or floor is 0); retry shortly") {
		t.Fatalf("scale from zero: %d %s", resp.StatusCode, out)
	}
}
