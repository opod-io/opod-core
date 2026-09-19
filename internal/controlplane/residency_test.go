package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
)

// A worker whose engine keeps installed models and loads on request reports
// both lists. The leader keeps the model ROUTABLE either way — the engine
// loads it on the first request, so dropping the row would stop traffic to a
// worker that can answer — and records what is not in memory as cold, which is
// what memory accounting reads. A worker that sends no resident list is, as
// before, taken to hold everything it lists.
func TestHeartbeatResidencyMarksColdRowsAndKeepsThemRoutable(t *testing.T) {
	srv, ts, workers := drainFixture(t)
	ctx := context.Background()

	// Over the wire, as a worker sends it: w1 answers for "m" and has nothing
	// in memory; w2 does not say.
	body, _ := json.Marshal(map[string]any{"id": "w1", "loaded_models": []string{"m"}, "resident_models": []string{}})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/v1/nodes/heartbeat", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+drainAdminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("heartbeat: %v %v", err, resp)
	}
	resp.Body.Close()

	cold := func(node string) bool {
		rows, _ := srv.store.Placements().GetByNode(ctx, node)
		if len(rows) != 1 || rows[0].ModelID != "m" || rows[0].Status != "ready" {
			t.Fatalf("%s: rows %+v, want one ready row for m", node, rows)
		}
		return rows[0].Cold
	}
	if !cold("w1") {
		t.Error("installed and not resident must be a cold row")
	}
	if cold("w2") {
		t.Error("a worker that reports no resident list holds what it lists")
	}

	// Cold is not "gone": with w2 drained, w1 still takes the request.
	if code, out := adminPost(t, ts, "/admin/v1/nodes/w2/drain"); code != http.StatusOK {
		t.Fatalf("drain: %d %s", code, out)
	}
	before := workers["w1"].chats.Load()
	if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("a cold placement must still be routed to: %d %s", resp.StatusCode, out)
	}
	if workers["w1"].chats.Load() != before+1 {
		t.Error("the request did not reach the worker holding the cold copy")
	}
	if code, out := adminPost(t, ts, "/admin/v1/nodes/w2/undrain"); code != http.StatusOK {
		t.Fatalf("undrain: %d %s", code, out)
	}

	// Memory facts: the cold copy holds no memory on w1; w2's does.
	for _, id := range []string{"w1", "w2"} {
		n, _ := srv.store.Nodes().Get(ctx, id)
		n.RAMGB = 64
		if err := srv.store.Nodes().Upsert(ctx, *n); err != nil {
			t.Fatal(err)
		}
	}
	cat := []models.Entry{{ID: "m", SizeBytes: 8 << 30}}
	facts, err := scheduler.WorkerMemoryFacts(ctx, srv.store, cat, "", 10, time.Minute, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	resident := map[string]int64{}
	for _, f := range facts {
		resident[f.NodeID] = f.ResidentBytes
	}
	if resident["w1"] != 0 || resident["w2"] == 0 {
		t.Errorf("resident bytes: %v, want nothing on w1 (cold) and the model's footprint on w2", resident)
	}

	// Resident again: the row is warm.
	loaded := []string{"m"}
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w1", LoadedModels: loaded, ResidentModels: &loaded}, Caller{Admin: true}); err != nil {
		t.Fatal(err)
	}
	if cold("w1") {
		t.Error("a model reported resident is not cold")
	}
}
