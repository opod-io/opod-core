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

	"github.com/opod-io/opod-sdk/nodeapi"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// T11.2 / ADR-065 — the park window.
//
// While a gang part's pod terminates its RPC process is already gone, but the
// worker keeps heartbeating through the grace period and its shard row still
// reads `ready`. The leader therefore believed the gang could serve, routed a
// request into a dead part, and answered the caller **502** — the one error
// class capacity.go's own header says must never happen, "an error a client
// cannot act on". Every park had this window and its width was the heartbeat
// bound; measured on the design-partner cell nine seconds after a park.
//
// The decision (G13) is that the worker says goodbye — a final heartbeat
// declaring its engine `stopped`, which is terminal by definition — and the
// leader acts on that ONE state. Silence stays the heartbeat rule's (C6), and
// `crash-looping` stays a report: it may be up again by the time the next
// request lands, and taking it out of rotation would flap the endpoint.
func TestATerminatingPartAnswers503NotAnUnactionable502(t *testing.T) {
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
	srv.plan.present, srv.plan.modelID = true, "m"

	// A two-part gang, both parts heartbeating, both shard rows ready.
	for _, id := range []string{"w1", "w2"} {
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, Hostname: id, Address: "10.0.0.1:8081", State: "ready", LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for _, sh := range []store.Shard{
		{ID: "s-m-g0-coord", ModelID: "m", GangID: "g0", Role: "coordinator", NodeID: "w1", Status: "ready"},
		{ID: "s-m-g0-rpc-0", ModelID: "m", GangID: "g0", Role: "rpc", NodeID: "w2", Status: "ready"},
	} {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	if !srv.modelServable(ctx, "m") {
		t.Fatal("a gang with both parts alive serves")
	}

	// The park begins: w2's pod gets SIGTERM, its rpc-server dies, and the
	// worker says so on its way out. Its heartbeat is still fresh and its
	// shard row still reads ready — that is the whole window.
	say(t, srv, "w2", nodeapi.EngineStopped, "worker shutting down")
	if srv.modelServable(ctx, "m") {
		t.Fatal("a gang whose part said its engine is gone cannot serve — the request must be refused, not routed into a dead part")
	}

	// And the refusal is the actionable one: 503 with Retry-After, never 502.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		bytes.NewReader([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
	req.Header.Set("Content-Type", "application/json")
	srv.dispatchOpenAIChat(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("a request in the park window is 503, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("…with Retry-After: the caller is being told to come back, not that we broke")
	}

	// A CRASH-LOOPING engine is a different statement and does not take the
	// part out: the process may be up again by the time the next request
	// lands, and flapping an endpoint between serving and 503 on a report is
	// worse than routing one request at a worker that then errors.
	say(t, srv, "w2", nodeapi.EngineCrashLooping, "exit status 1")
	if !srv.modelServable(ctx, "m") {
		t.Error("a crash-looping report keeps its place in rotation (C6): only `stopped` is terminal")
	}

	// A stale statement counts for nothing — silence belongs to the heartbeat
	// rule, which is the line C6 drew and this row does not move.
	srv.nodeEngine.Store("w2", nodeEngineSample{
		EngineState: nodeapi.EngineState{State: nodeapi.EngineStopped},
		at:          time.Now().Add(-10 * time.Minute),
	})
	if !srv.modelServable(ctx, "m") {
		t.Error("a stale engine sample is not a statement about now")
	}
	if why := srv.engineGoneWhy("w2"); why != "" {
		t.Errorf("…and says nothing: %q", why)
	}
}

// The same rule on the plain path: a single-worker endpoint whose worker is
// terminating answers 503, and /loadz stops counting that worker as capacity
// while still reporting the card it holds.
func TestAWorkerThatSaidGoodbyeIsNotCapacity(t *testing.T) {
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
	srv.plan.present, srv.plan.modelID = true, "m"

	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", Address: "10.0.0.1:8081", State: "ready", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := st.Placements().Upsert(ctx, store.Placement{NodeID: "w1", ModelID: "m", Status: "ready", LastSeen: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if !srv.modelServable(ctx, "m") {
		t.Fatal("a heartbeating holder serves")
	}
	if n := loadzWorkers(t, srv); n != 1 {
		t.Fatalf("one worker counted: %d", n)
	}

	say(t, srv, "w1", nodeapi.EngineStopped, "worker shutting down")
	if srv.modelServable(ctx, "m") {
		t.Error("a worker on its way out is not a holder the caller can be sent to")
	}
	if n := loadzWorkers(t, srv); n != 0 {
		t.Errorf("…and not capacity an autoscaler should count either: workers=%d", n)
	}
	// The card it still holds is not hidden: that is what this number is for.
	if unhealthy, worst := srv.EnginesUnhealthy(); unhealthy != 1 || !strings.Contains(worst, "stopped") {
		t.Errorf("a stopped engine still holding a card is reported: %d %q", unhealthy, worst)
	}
}

// say records what a worker reported about its engine, as a heartbeat would.
func say(t *testing.T, srv *Server, node, state, detail string) {
	t.Helper()
	srv.nodeEngine.Store(node, nodeEngineSample{
		EngineState: nodeapi.EngineState{State: state, Detail: detail, Since: time.Now().UTC().Format(time.RFC3339)},
		at:          time.Now(),
	})
}

func loadzWorkers(t *testing.T, srv *Server) int {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
	var body struct {
		Workers int `json:"workers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Workers
}
