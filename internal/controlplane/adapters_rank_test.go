package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// POST /admin/v1/adapters carries the adapter's rank to the workers, and a
// worker's refusal ("started with --max-lora-rank 16") comes back per worker
// in the result instead of being flattened into a gateway error.
func TestAdminAdapterRankReachesTheWorker(t *testing.T) {
	ctx := context.Background()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var seen []map[string]any
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got map[string]any
		_ = json.NewDecoder(r.Body).Decode(&got)
		seen = append(seen, got)
		if rank, _ := got["rank"].(float64); rank > 16 {
			http.Error(w, `adapter "legal" has rank 64, but this worker's engine was started with --max-lora-rank 16`, http.StatusConflict)
			return
		}
		_, _ = io.WriteString(w, `{"status":"ready"}`)
	}))
	defer worker.Close()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Address: strings.TrimPrefix(worker.URL, "http://"), WorkerToken: "tok",
		State: store.NodeStateReady, LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Placements().Upsert(ctx, store.Placement{NodeID: "w1", ModelID: "qwen", Status: "ready", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	orch := scheduler.New(st, nil, log, "")
	cfg := config.Default()
	cfg.Listen = ":0"
	srv := NewServer(cfg, st, &stubLeaderEngine{}, nil, log, orch)
	srv.plan.modelID, srv.plan.present = "qwen", true
	ts := httptest.NewServer(srv.routes())
	defer ts.Close()
	plain, rec, err := auth.Generate("adapter-rank-admin", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.APIKeys().Create(ctx, rec); err != nil {
		t.Fatal(err)
	}
	post := func(body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/v1/adapters", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+plain)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, out := post(`{"name":"support","source":"hf/support-lora","rank":8}`); code != http.StatusOK {
		t.Fatalf("rank 8: %d %s", code, out)
	}
	if code, out := post(`{"name":"plain","source":"hf/plain-lora"}`); code != http.StatusOK {
		t.Fatalf("no rank: %d %s", code, out)
	}
	code, out := post(`{"name":"legal","source":"hf/legal-lora","rank":64}`)
	if code != http.StatusBadGateway || !strings.Contains(out, "rank 64") || !strings.Contains(out, "--max-lora-rank 16") {
		t.Fatalf("the worker's refusal must come back with both numbers: %d %s", code, out)
	}
	if code, _ := post(`{"name":"neg","source":"hf/x","rank":-4}`); code != http.StatusBadRequest {
		t.Fatalf("negative rank: %d, want 400", code)
	}
	if len(seen) != 3 {
		t.Fatalf("the worker saw %d loads, want 3 (the negative rank never leaves the leader)", len(seen))
	}
	if seen[0]["rank"] != float64(8) || seen[2]["rank"] != float64(64) {
		t.Errorf("rank did not reach the worker: %v", seen)
	}
	if _, present := seen[1]["rank"]; present {
		t.Errorf("an unstated rank must omit the key: %v", seen[1])
	}
}
