package controlplane

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/events"
	"github.com/opod-io/opod/internal/store"
)

// E2: one record() → one event-log row AND one bus event on the family topic.
func TestRecordEmitsOnceToLogAndBus(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ch, cancel := srv.bus.Subscribe(8)
	defer cancel()
	srv.record("node.registered", "w9", map[string]any{"hostname": "w9"})
	select {
	case ev := <-ch:
		if ev.Topic != events.TopicNodes || ev.ID != "w9" {
			t.Fatalf("bus event %+v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("no bus event")
	}
	rows, err := st.EventLog().After(context.Background(), 0, 10)
	if err != nil || len(rows) != 1 || rows[0].Type != "node.registered" || rows[0].Subject != "w9" {
		t.Fatalf("event log %v %+v", err, rows)
	}
	for typ, want := range map[string]events.Topic{"model.loaded": events.TopicModels, "shard.created": events.TopicShards, "auth.updated": events.TopicAudit, "worker.sleep": events.TopicNodes} {
		if got := topicFor(typ); got != want {
			t.Fatalf("topicFor(%s) = %s, want %s", typ, got, want)
		}
	}
}
