package router

import (
	"context"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// A gang whose engine is ONE distributed process — SGLang's launcher, vLLM over
// Ray — serves its API from rank 0 alone. The other ranks hold their share of
// the layers and answer no HTTP, so their worker's engine probe can never
// succeed and `EngineSilentSince` is set within a minute of the gang forming.
// Judging those parts by the engine-silence rule took the WHOLE gang out of
// rotation: on the design-partner cell (2026-09-22) a two-machine SGLang gang
// answered the control plane's probe in 713 ms and was 502 by the next request.
//
// A part is judged on whether its WORKER is there. The head keeps the full
// rule — that one really must serve.
func TestAGangPartIsNotJudgedByAnEngineItNeverRuns(t *testing.T) {
	const bound = 30 * time.Second
	now := time.Now()
	silentPart := store.Node{
		ID: "rank1", State: store.NodeStateReady, LastHeartbeat: now,
		EngineSilentSince: now.Add(-10 * time.Minute), // it has NEVER answered
	}
	if silentPart.TakesNewWork(bound, now) {
		t.Fatal("the full rule must still refuse an engine-silent node — that is what it is for")
	}
	if !silentPart.TakesPartWork(bound, now) {
		t.Errorf("a non-head part answers no HTTP by design and must still count: %q",
			silentPart.WhyNoPartWork(bound, now))
	}

	// What DOES stop a gang through one of its parts: the worker going away,
	// and an operator draining it.
	gone := store.Node{ID: "rank1", State: store.NodeStateReady, LastHeartbeat: now.Add(-5 * time.Minute)}
	if gone.TakesPartWork(bound, now) {
		t.Error("a part whose worker stopped heartbeating stops its gang")
	}
	drained := store.Node{ID: "rank1", State: store.NodeStateDraining, LastHeartbeat: now}
	if drained.TakesPartWork(bound, now) {
		t.Error("an operator's drain outlives everything else")
	}
}

// The same judgement through the router: the gang is routable although its
// non-head part's engine has been silent for ever.
func TestShardGroupRoutableIgnoresANonHeadPartsEngine(t *testing.T) {
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	now := time.Now()
	for _, n := range []store.Node{
		{ID: "head", State: store.NodeStateReady, LastHeartbeat: now, Address: "10.0.0.1:8081"},
		{ID: "rank1", State: store.NodeStateReady, LastHeartbeat: now, Address: "10.0.0.2:8081",
			EngineSilentSince: now.Add(-10 * time.Minute)},
	} {
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	r := &Router{store: st, heartbeatMaxAge: 30 * time.Second}
	parts := []store.Shard{
		{ID: "c", Role: "coordinator", NodeID: "head", Status: "ready"},
		{ID: "r1", Role: "rank", NodeID: "rank1", Status: "ready"},
	}
	if !r.shardGroupRoutable(ctx, parts) {
		t.Fatal("a gang whose non-head part answers no HTTP is still routable")
	}
	// And the head is not exempt: an engine-silent HEAD is the news the rule
	// exists for, because that is the process this router dials.
	head, _ := st.Nodes().Get(ctx, "head")
	head.EngineSilentSince = now.Add(-10 * time.Minute)
	if err := st.Nodes().Upsert(ctx, *head); err != nil {
		t.Fatal(err)
	}
	if r.shardGroupRoutable(ctx, parts) {
		t.Error("a gang whose HEAD's engine stopped answering is not routable")
	}
}
