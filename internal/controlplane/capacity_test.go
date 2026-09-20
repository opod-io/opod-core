package controlplane

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
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

// A gang parked at floor 0 is ABSENT, not broken, and a gang's part count is
// its own. Both halves were wrong on the design-partner cell (2026-09-20): a
// two-part gang scaled to zero by the autoscaler answered "3 part(s) stopped
// heartbeating — waiting for it to return or be replaced", which counts a
// sibling gang's parts and sends the caller looking for a human to fix a gang
// that is coming back by itself.
func TestParkedGangIsWakingNotBroken(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, log, nil)

	live, gone := time.Now(), time.Now().Add(-time.Hour)
	node := func(id string, beat time.Time) {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: beat}); err != nil {
			t.Fatal(err)
		}
	}
	part := func(id, gang, role, node string) {
		if err := st.Shards().Create(ctx, store.Shard{ID: id, ModelID: "sharded", GangID: gang, Role: role,
			NodeID: node, Address: node + ":50052", Status: "ready", CreatedAt: live, LastSeen: live}); err != nil {
			t.Fatal(err)
		}
	}

	// One gang of two parts, both gone: the whole gang is parked.
	node("n1", gone)
	node("n2", gone)
	part("c0", "g0", "coordinator", "n1")
	part("p0", "g0", "rpc", "n2")

	got := srv.gangReason(ctx, "sharded")
	if got != wakingMessage {
		t.Errorf("a parked gang must read as waking, got %q", got)
	}
	if strings.Contains(got, "stopped heartbeating") || strings.Contains(got, "be replaced") {
		t.Error("a parked gang must not be described as one waiting for a human")
	}

	// A SECOND gang, also fully down, must not inflate the first's count —
	// and must not turn an absence into a degradation.
	node("n3", gone)
	node("n4", gone)
	part("c1", "g1", "coordinator", "n3")
	part("p1", "g1", "rpc", "n4")
	if got := srv.gangReason(ctx, "sharded"); got != wakingMessage {
		t.Errorf("two parked gangs are still an absence, got %q", got)
	}

	// Now g1 holds its coordinator and has lost only its rpc part. THAT is a
	// degraded gang, and the count is g1's own — never the four rows stored.
	node("n3", live)
	got = srv.gangReason(ctx, "sharded")
	if !strings.Contains(got, "1 part(s) stopped heartbeating") {
		t.Errorf("a degraded gang reports its OWN lost parts, got %q", got)
	}
	for _, wrong := range []string{"2 part(s)", "3 part(s)", "4 part(s)"} {
		if strings.Contains(got, wrong) {
			t.Errorf("part count crossed a gang boundary: %q contains %q", got, wrong)
		}
	}
}
