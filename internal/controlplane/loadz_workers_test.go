package controlplane

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/opod-io/opod-sdk/adminapi"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// /loadz `workers` is capacity an autoscaler can count on: workers that can
// take a NEW request for the plan's model. A drained worker, one whose engine
// sleeps and one that holds another model are alive, and are not that — and
// their pressure is not `reporting` either. Undrain brings the worker back.
func TestLoadzCountsOnlyWorkersThatTakeNewRequests(t *testing.T) {
	srv, _, _ := drainFixture(t) // w1 and w2, both serving "m"
	ctx := context.Background()
	admin := Caller{Admin: true}
	srv.plan.modelID, srv.plan.present = "m", true
	load := &engines.EngineLoad{KVUsedPct: 80, QueueDepth: 4, TokensPerSec: 10}
	for _, id := range []string{"w1", "w2"} {
		if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: id, LoadedModels: []string{"m"}, Load: load}, admin); err != nil {
			t.Fatal(err)
		}
	}
	loadz := func() adminapi.Load {
		rec := httptest.NewRecorder()
		srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
		var out adminapi.Load
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return out
	}
	if ld := loadz(); ld.Workers != 2 || ld.Reporting != 2 || ld.QueueDepth != 8 {
		t.Fatalf("two serving workers: %+v", ld)
	}

	if err := srv.DrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if ld := loadz(); ld.Workers != 1 || ld.Reporting != 1 || ld.QueueDepth != 4 {
		t.Fatalf("a drained worker is not capacity and its queue is not pressure: %+v", ld)
	}
	if err := srv.UndrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if ld := loadz(); ld.Workers != 2 {
		t.Fatalf("after undrain: %+v", ld)
	}

	// w2's engine sleeps: resident, alive, takes no request.
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w2", LoadedModels: []string{"m"}, Load: load, Sleeping: true}, admin); err != nil {
		t.Fatal(err)
	}
	if ld := loadz(); ld.Workers != 1 || ld.Reporting != 1 {
		t.Fatalf("a sleeping worker is not capacity: %+v", ld)
	}
	// w2 serves some other model: not capacity for the plan's.
	if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w2", LoadedModels: []string{"other"}, Load: load}, admin); err != nil {
		t.Fatal(err)
	}
	if ld := loadz(); ld.Workers != 1 {
		t.Fatalf("a worker holding another model is not capacity for the plan's: %+v", ld)
	}
	// A placement the leader is draining is not routable either.
	if err := srv.store.Placements().SetStatus(ctx, "w1", "m", store.PlacementDraining); err != nil {
		t.Fatal(err)
	}
	if ld := loadz(); ld.Workers != 0 || ld.Reporting != 0 || ld.KVUsedPct != 0 {
		t.Fatalf("nothing can take a request: %+v", ld)
	}
}
