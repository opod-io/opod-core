package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

func listedModels(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp, err := http.Get(ts.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &out); err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models: %d %s", resp.StatusCode, b)
	}
	ids := make([]string, 0, len(out.Data))
	for _, m := range out.Data {
		ids = append(ids, m.ID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// GET /v1/models lists what a request can be answered for now: a model held
// only by a drained or lost worker is not on it (picking it would be a 503),
// and it is back the moment the worker is.
func TestModelsListFollowsWhoTakesNewWork(t *testing.T) {
	srv, ts, _ := drainFixture(t) // w1 and w2 both serve "m"
	ctx := context.Background()
	admin := Caller{Admin: true}
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w1", LoadedModels: []string{"m", "solo"}}, admin); err != nil {
		t.Fatal(err)
	}
	if got := listedModels(t, ts); got != "m,solo" {
		t.Fatalf("both workers serving: %q", got)
	}
	if err := srv.DrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if got := listedModels(t, ts); got != "m" {
		t.Fatalf("w1 drained — solo is held by nobody who takes requests: %q", got)
	}
	if err := srv.UndrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if got := listedModels(t, ts); got != "m,solo" {
		t.Fatalf("after undrain: %q", got)
	}
	// w1's heartbeats stop: the rows stay, the model does not.
	if err := srv.store.Nodes().Heartbeat(ctx, "w1", time.Now().Add(-10*time.Minute), ""); err != nil {
		t.Fatal(err)
	}
	if got := listedModels(t, ts); got != "m" {
		t.Fatalf("w1 lost: %q", got)
	}
	// A placement the leader is draining is not listed; the node's others are.
	if err := srv.store.Nodes().Heartbeat(ctx, "w1", time.Now(), ""); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.Placements().SetStatus(ctx, "w1", "solo", store.PlacementDraining); err != nil {
		t.Fatal(err)
	}
	if got := listedModels(t, ts); got != "m" {
		t.Fatalf("solo's only placement is draining: %q", got)
	}
}

// What is merely asleep stays listed, because asking for it is how it wakes:
// a sleeping placement on a live worker, and the plan's model while the plan
// parks it by design (floor 0) — also with no worker at all.
func TestModelsListKeepsWhatWakesOnDemand(t *testing.T) {
	srv, ts, _ := drainFixture(t)
	ctx := context.Background()
	admin := Caller{Admin: true}
	for _, id := range []string{"w1", "w2"} {
		if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: id, LoadedModels: []string{"m"}, Sleeping: true}, admin); err != nil {
			t.Fatal(err)
		}
	}
	if got := listedModels(t, ts); got != "m" {
		t.Fatalf("every worker asleep — the model wakes on demand and stays listed: %q", got)
	}
	resp, _ := chat(t, ts, drainChatBody, "")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("the waking path must still answer 503 + Retry-After: %d", resp.StatusCode)
	}

	// Scale to zero: no worker left, the plan says floor 0.
	for _, id := range []string{"w1", "w2"} {
		if err := srv.RemoveNode(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := listedModels(t, ts); got != "" {
		t.Fatalf("no plan, no worker: %q", got)
	}
	srv.plan.mu.Lock()
	srv.plan.present, srv.plan.zeroFloor, srv.plan.modelID = true, true, "m"
	srv.plan.mu.Unlock()
	if got := listedModels(t, ts); got != "m" {
		t.Fatalf("a parked endpoint lists its model: %q", got)
	}
	// Not by design (floor above zero, workers gone): not listed.
	srv.plan.mu.Lock()
	srv.plan.zeroFloor = false
	srv.plan.mu.Unlock()
	if got := listedModels(t, ts); got != "" {
		t.Fatalf("a plan whose workers are simply gone lists nothing: %q", got)
	}
}

// A sharded model is its whole gang: a part on a drained node takes it off.
func TestModelsListDropsAGangWithAPartDown(t *testing.T) {
	srv, ts, _ := drainFixture(t)
	ctx, now := context.Background(), time.Now()
	st := srv.store
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "local", Hostname: "leader", State: store.NodeStateReady, LastHeartbeat: now}); err != nil {
		t.Fatal(err)
	}
	if err := st.Placements().Upsert(ctx, store.Placement{NodeID: "local", ModelID: "g", Status: "ready", LastSeen: now}); err != nil {
		t.Fatal(err)
	}
	for _, sh := range []store.Shard{
		{ID: "c", ModelID: "g", Role: "coordinator", NodeID: "w1", Address: "192.0.2.1:9001", Status: "ready", CreatedAt: now, LastSeen: now},
		{ID: "p", ModelID: "g", Role: "rpc", NodeID: "w2", Address: "192.0.2.2:50052", Status: "ready", CreatedAt: now, LastSeen: now},
	} {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	if got := listedModels(t, ts); got != "g,m" {
		t.Fatalf("gang up: %q", got)
	}
	if err := srv.DrainNode(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	if got := listedModels(t, ts); got != "m" {
		t.Fatalf("a gang with a part on a drained node must leave the list: %q", got)
	}
}
