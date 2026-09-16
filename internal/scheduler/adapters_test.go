package scheduler

// R15.15 · adding a LoRA at runtime is a fan-out over the workers that hold the
// base, and the interesting cases are all about honesty:
//   - one worker refusing does not read as success, because the model id would
//     then answer on some workers and 404 on others;
//   - a node that holds the base but is draining is skipped, not fatal — an
//     adapter must be addable while one node is unhealthy;
//   - no holder at all is a refusal with a reason, not an empty success.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/store"
)

func adapterWorker(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status >= 400 {
			http.Error(w, "engine refused the adapter: rank 64 exceeds --max-lora-rank 16", status)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func seedHolder(t *testing.T, st store.Store, id, addr, state, model string) {
	t.Helper()
	ctx := context.Background()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Address: strings.TrimPrefix(addr, "http://"), WorkerToken: "tok", State: state}); err != nil {
		t.Fatal(err)
	}
	if err := st.Placements().Upsert(ctx, store.Placement{NodeID: id, ModelID: model, Status: "ready"}); err != nil {
		t.Fatal(err)
	}
}

func adapterOrch(t *testing.T) (*Orchestrator, store.Store) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	return &Orchestrator{Store: st, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HTTP: http.DefaultClient}, st
}

func TestOneWorkerRefusingIsReportedPerNode(t *testing.T) {
	o, st := adapterOrch(t)
	seedHolder(t, st, "n1", adapterWorker(t, 200).URL, "ready", "qwen")
	seedHolder(t, st, "n2", adapterWorker(t, 502).URL, "ready", "qwen")

	res, err := o.LoadAdapter(context.Background(), "qwen", "support", "hf/support-lora")
	if err != nil {
		t.Fatalf("the call itself failed: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("expected an answer per holder, got %+v", res)
	}
	byNode := map[string]AdapterResult{}
	for _, r := range res {
		byNode[r.Node] = r
	}
	if !byNode["n1"].OK {
		t.Fatalf("the worker that accepted reads as failed: %+v", byNode["n1"])
	}
	if byNode["n2"].OK {
		t.Fatal("a worker that refused reads as ok — the model id would 404 there")
	}
	if !strings.Contains(byNode["n2"].Error, "max-lora-rank") {
		t.Fatalf("the engine's reason was dropped: %q", byNode["n2"].Error)
	}
}

func TestADrainingHolderIsSkippedNotFatal(t *testing.T) {
	o, st := adapterOrch(t)
	seedHolder(t, st, "n1", adapterWorker(t, 200).URL, "ready", "qwen")
	seedHolder(t, st, "n2", "http://127.0.0.1:1", "draining", "qwen")

	res, err := o.LoadAdapter(context.Background(), "qwen", "support", "hf/support-lora")
	if err != nil {
		t.Fatalf("one draining node stopped the whole fan-out: %v", err)
	}
	if len(res) != 1 || res[0].Node != "n1" || !res[0].OK {
		t.Fatalf("expected the ready holder alone to answer, got %+v", res)
	}
}

func TestNoHolderIsRefusedWithAReason(t *testing.T) {
	o, _ := adapterOrch(t)
	_, err := o.LoadAdapter(context.Background(), "qwen", "support", "hf/support-lora")
	if err == nil {
		t.Fatal("loading an adapter with no worker holding the base read as success")
	}
	if !strings.Contains(err.Error(), "qwen") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
}
