package leader

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"testing"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// BenchmarkHeartbeatNodeEightModels — allocs/op is the figure (PLAN T15.12):
// each of the eight reported models used to rebuild the whole catalog as a
// fresh []engines.Source, 47 entries, every heartbeat.
func BenchmarkHeartbeatNodeEightModels(b *testing.B) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	defer st.Close()
	cat := make([]models.Entry, 47)
	for i := range cat {
		cat[i] = models.Entry{ID: fmt.Sprintf("m%d", i)}
		cat[i].Source.Repo = fmt.Sprintf("org/m%d", i)
		cat[i].Source.OllamaName = fmt.Sprintf("m%d:latest", i)
	}
	srv := NewServer(cfg, st, &stubLeaderEngine{}, cat, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx := context.Background()
	if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: "w1", Hostname: "w1", Address: "127.0.0.1:1"}, Caller{Admin: true}, "tok"); err != nil {
		b.Fatal(err)
	}
	loaded := make([]string, 8)
	for i := range loaded {
		loaded[i] = fmt.Sprintf("org/m%d", i)
	}
	req := HeartbeatRequest{ID: "w1", LoadedModels: loaded}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := srv.HeartbeatNode(ctx, req, Caller{Admin: true}); err != nil {
			b.Fatal(err)
		}
	}
}
