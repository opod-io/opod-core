package router

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// A worker whose pod is replaced keeps its row, and every pick walked past it
// with a WARN — one line per request, per dead row, for as long as the leader
// ran. The warning belongs to the moment a worker goes stale: once, and again
// only if it came back and went stale a second time.
func TestAStaleWorkerIsWarnedAboutOncePerEpisode(t *testing.T) {
	r, st := drainRouter(t,
		store.Node{ID: "live", State: store.NodeStateReady},
		store.Node{ID: "gone", State: store.NodeStateReady})
	var buf bytes.Buffer
	r.log = slog.New(slog.NewTextHandler(&buf, nil))
	r.SetHeartbeatMaxAge(30 * time.Second)
	ctx := context.Background()
	warnings := func() int { return strings.Count(buf.String(), "router skipping stale worker") }

	if err := st.Nodes().Heartbeat(ctx, "gone", time.Now().Add(-5*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, node, _ := r.pick(ctx, "m"); node == "gone" {
			t.Fatalf("pick %d chose the stale worker", i)
		}
	}
	if n := warnings(); n != 1 {
		t.Fatalf("50 picks past one stale worker: %d warnings, want 1", n)
	}

	// It heartbeats again (a restarted process on the same row), then goes quiet again.
	if err := st.Nodes().Heartbeat(ctx, "gone", time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		r.pick(ctx, "m")
	}
	if err := st.Nodes().Heartbeat(ctx, "gone", time.Now().Add(-5*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		r.pick(ctx, "m")
	}
	if n := warnings(); n != 2 {
		t.Fatalf("a second stale episode is a second warning: %d, want 2", n)
	}
}
