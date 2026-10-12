package leader

// A failed gang create cleans up only what IT wrote, and says nothing alarming
// when there is nothing left to clean.
//
// Measured on the design-partner cell (2026-10-12): a create ended because its
// part's worker was replaced; the leader's reincarnation sweep had already
// removed the gang 0.3 ms earlier, and the create's own cleanup — a teardown
// of the gang's CURRENT rows, run after the create's guard was released —
// logged `shards/create cleanup failed … has no gang "g1"` at ERROR. Harmless
// that time; but the same cleanup reads whatever rows the gang has by then,
// so it could as well remove a gang the heal loop had just formed under the
// same id. The create's own rollback already stops and deletes exactly the
// parts it started, while it still holds the guard.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// syncBuffer is a log sink the create's goroutine and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// failCreateByReplacingItsWorker starts a create of g1 on n1, waits for its
// coordinator to start, runs during (the moment a sibling writer acts), then
// replaces n1's worker so the create ends with ErrPartLost.
func failCreateByReplacingItsWorker(t *testing.T, srv *Server, w *formingWorker, model string, during func()) error {
	t.Helper()
	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- srv.CreateShards(ctx, CreateShardsRequest{ModelID: model, Gang: "g1", Shards: 1, Nodes: []string{"n1"}})
	}()
	select {
	case <-w.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the coordinator was never started")
	}
	during()
	w.mu.Lock()
	w.gone = true
	w.mu.Unlock()
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "n1", BootID: "boot-b"}, Caller{Admin: true}); err != nil {
		t.Fatal(err)
	}
	err := awaitCreate(t, done, 10*time.Second)
	if !errors.Is(err, scheduler.ErrPartLost) {
		t.Fatalf("the create must end because its part's worker went away: %v", err)
	}
	return err
}

func TestAFailedCreateLeavesAGangItDidNotWrite(t *testing.T) {
	srv, _, w, entry := formationFixture(t)
	ctx := context.Background()
	if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: "n2", Address: strings.TrimPrefix(w.srv.URL, "http://"), BootID: "boot-n2"}, Caller{Admin: true}, "tok"); err != nil {
		t.Fatal(err)
	}
	// Rows of g1 this create never wrote: the gang formed under the same id by
	// another writer, on another node, while the failing create still ran.
	at := time.Now().Add(time.Second)
	theirs := store.Shard{ID: "s-" + entry.ID + "-g1-coord", ModelID: entry.ID, GangID: "g1", Role: "coordinator",
		NodeID: "n2", Address: "192.0.2.9:9001", ProcessID: "s-" + entry.ID + "-g1-coord", Status: "ready", CreatedAt: at, LastSeen: at}

	failCreateByReplacingItsWorker(t, srv, w, entry.ID, func() {
		if err := srv.store.Shards().Create(ctx, theirs); err != nil {
			t.Fatal(err)
		}
	})

	got, err := srv.store.Shards().GetByGang(ctx, entry.ID, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Address != theirs.Address {
		t.Fatalf("a failed create removed a gang it did not write: rows left %+v", got)
	}
}

func TestAFailedCreateWithNothingLeftLogsNoError(t *testing.T) {
	srv, _, w, entry := formationFixture(t)
	var logs syncBuffer
	srv.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError}))

	failCreateByReplacingItsWorker(t, srv, w, entry.ID, func() {})

	if out := logs.String(); strings.Contains(out, "cleanup failed") || strings.Contains(out, "has no gang") {
		t.Fatalf("a create whose parts are already gone must not log a failed cleanup: %s", out)
	}
}
