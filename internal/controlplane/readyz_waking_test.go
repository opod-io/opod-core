package controlplane

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// T11.5 — /readyz must not call a leader `sleeping` while its workers are up.
//
// With autoscale floor 0 the leader answered {"mode":"sleeping"} whenever
// nothing could serve, and it asked that BEFORE it asked whether a gang was
// serving. On the design-partner cell (2026-09-20) that produced two wrong
// answers from one branch: a two-part gang that was answering requests
// reported itself parked, and — during the run that failed — both parts were
// Running and registered, the gang could not form and never would have, and
// the leader still reported a healthy parked endpoint.
//
// Parked (no workers) and awake-but-not-serving (workers registered, no gang)
// are different states and only the first is by design. Both stay 200: taking
// the leader out of the Service would turn the honest "503 waking" into a
// connection error, and the waking request is what brings the workers back.
func TestReadyzSeparatesParkedFromWaking(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Router.HeartbeatMaxAgeSeconds = 30
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, log, nil)
	// A mounted plan with floor 0: zero capacity is deliberate.
	srv.plan.present, srv.plan.zeroFloor, srv.plan.modelID = true, true, "m"

	readyz := func() (int, string) {
		t.Helper()
		rec := httptest.NewRecorder()
		srv.readyz(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		mode, _ := body["mode"].(string)
		return rec.Code, mode
	}

	// 1 · parked: no workers at all. Healthy by design.
	if code, mode := readyz(); code != http.StatusOK || mode != "sleeping" {
		t.Fatalf("a parked floor-0 endpoint is sleeping: %d/%s", code, mode)
	}

	// 2 · the parts come back and register, but no gang has formed. The
	// endpoint cannot serve and Kubernetes must not be told it is parked.
	for _, id := range []string{"w1", "w2"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	if code, mode := readyz(); code != http.StatusOK || mode != "waking" {
		t.Fatalf("registered parts with no gang are waking, not sleeping: %d/%s", code, mode)
	}

	// 3 · the gang forms. A serving gang reports what is serving it — the
	// floor-0 branch used to answer `sleeping` over the top of this.
	gang := []store.Shard{
		{ID: "s-m-g0-coord", ModelID: "m", GangID: "g0", Role: "coordinator", NodeID: "w1", Status: "ready"},
		{ID: "s-m-g0-rpc-0", ModelID: "m", GangID: "g0", Role: "rpc", NodeID: "w2", Status: "ready"},
	}
	for _, sh := range gang {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	if code, mode := readyz(); code != http.StatusOK || mode != "shard-coordinator" {
		t.Fatalf("a serving gang is not parked: %d/%s", code, mode)
	}

	// 4 · the workers go away again (heartbeats stop). Back to parked, even
	// though the shard rows still read `ready` — liveness is derived, and a
	// lost part is not capacity.
	for _, id := range []string{"w1", "w2"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, State: "ready", LastHeartbeat: time.Now().Add(-5 * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	if code, mode := readyz(); code != http.StatusOK || mode != "sleeping" {
		t.Fatalf("workers gone again is parked: %d/%s", code, mode)
	}
}
