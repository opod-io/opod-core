package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// `opod model add <id> --node <n>` on a worker whose engine serves one model
// per process stopped whatever that worker was serving — silently: the load
// succeeded and the other model's requests started to fail. The leader knows
// (LoadWouldReplace, which `model move` already asks), so it refuses and names
// both models; `--force` is the operator saying "replace it".
func TestPlaceOnNodesRefusesToReplaceWhatAOneModelWorkerServes(t *testing.T) {
	var loads atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		loads.Add(1)
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	}))
	defer worker.Close()
	addr := strings.TrimPrefix(worker.URL, "http://")

	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	for _, n := range []store.Node{
		{ID: "busy", Address: addr, WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: now, HardwareJSON: `{"Engine":"vllm"}`},
		{ID: "idle", Address: addr, WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: now, HardwareJSON: `{"Engine":"vllm"}`},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Placements().Upsert(ctx, store.Placement{NodeID: "busy", ModelID: "served-today", Status: "ready", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	entry := models.Entry{ID: "new-model", Source: models.SourceSpec{Type: "huggingface", Repo: "example/New"}}

	// Refused before ANY worker is touched — the idle one too: half a placement is worse than none.
	err = o.PlaceOnNodes(ctx, entry, []string{"idle", "busy"}, false, false)
	if !errors.Is(err, ErrUnplaceable) || !strings.Contains(err.Error(), "served-today") || !strings.Contains(err.Error(), "new-model") {
		t.Fatalf("want a refusal naming both models, got %v", err)
	}
	if loads.Load() != 0 {
		t.Fatalf("a refused placement still sent %d load(s)", loads.Load())
	}

	if err := o.PlaceOnNodes(ctx, entry, []string{"idle", "busy"}, false, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if loads.Load() != 2 {
		t.Fatalf("forced placement sent %d loads, want 2", loads.Load())
	}
}
