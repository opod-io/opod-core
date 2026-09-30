package leader

import (
	"context"
	"net/http"
	"testing"
)

// A worker that registers again at a NEW address is dialled there on the next
// request. The router's client for a node id was built from the first
// registration and cached by id, so a worker pod re-created under a pinned id
// (every certificate-identified worker, R9.6) kept being dialled at its
// previous pod's IP until the leader restarted — heartbeats 200, every chat
// "worker could not be reached" (design-partner cell, 2026-09-29, PLAN T7.3).
func TestAReregisteredWorkerIsDialledAtItsNewAddress(t *testing.T) {
	srv, ts, workers := drainFixture(t)
	ctx := context.Background()
	// Only w1 serves, so every chat is dialled to it — and cached.
	if err := srv.store.Placements().ReplaceForNode(ctx, "w2", nil); err != nil {
		t.Fatal(err)
	}
	if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("baseline chat: %d %s", resp.StatusCode, out)
	}
	if workers["w1"].chats.Load() != 1 {
		t.Fatalf("baseline: w1 saw %d chats", workers["w1"].chats.Load())
	}
	// The same node id comes back as a new process at a new address; the old
	// process is gone.
	moved := newFakeWorker(t)
	workers["w1"].Close()
	if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: "w1", Hostname: "w1", Address: moved.URL, BootID: "boot-2"}, Caller{Admin: true}, "tok-2"); err != nil {
		t.Fatal(err)
	}
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w1", LoadedModels: []string{"m"}}, Caller{Admin: true}); err != nil {
		t.Fatal(err)
	}
	if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("chat after the re-register: %d %s", resp.StatusCode, out)
	}
	if moved.chats.Load() != 1 {
		t.Fatalf("the re-registered worker saw %d chats: the leader is still dialling the old address", moved.chats.Load())
	}
}
