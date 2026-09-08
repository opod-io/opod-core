package controlplane

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// E1: the model / shard services answer typed errors without an HTTP request.
func TestModelAndShardServicesTypedErrors(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ctx := context.Background()
	if _, err := srv.AddModel(ctx, AddModelRequest{ID: "no-such-model"}); !errors.Is(err, ErrNoCatalogEntry) {
		t.Fatalf("unknown id: %v", err)
	}
	if err := srv.CreateShards(ctx, CreateShardsRequest{ModelID: "no-such-model"}); !errors.Is(err, ErrNoCatalogEntry) {
		t.Fatalf("shards for an unknown id: %v", err)
	}
	if err := srv.RemoveShards(ctx, "m"); !errors.Is(err, ErrNoOrchestrator) {
		t.Fatalf("no orchestrator: %v", err)
	}
	// a local model row is removed from the store (engine delete is best-effort)
	if err := st.Models().Upsert(ctx, store.Model{ID: "m", CatalogID: "m", Source: "llamacpp:repo/x", Status: "ready"}); err != nil {
		t.Fatal(err)
	}
	out, err := srv.DeleteModel(ctx, "m")
	if err != nil || out.Kind != "local" {
		t.Fatalf("delete local: %v %+v", err, out)
	}
	if m, _ := st.Models().Get(ctx, "m"); m != nil {
		t.Fatal("model row must be gone")
	}
	if got := sourceName("vllm:org/repo", "x"); got != "org/repo" {
		t.Fatalf("sourceName %q", got)
	}
	if got := sourceName("bare", "x"); got != "x" {
		t.Fatalf("sourceName fallback %q", got)
	}
}
