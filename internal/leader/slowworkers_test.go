package leader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/store"
	"github.com/opod-io/opod-core/pkg/adminapi"
)

// The policy's routing.tokenWeight and routing.slowFactor reach the router
// (T28.10, T28.12), and 0 — an absent field — turns each off again.
func TestThePolicyAppliesTokenWeightAndSlowFactor(t *testing.T) {
	srv, _ := slowTestServer(t)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "on", Routing: PolicyRouting{TokenWeight: 0.5, SlowFactor: 2}})
	if w, f := srv.router.TokenWeight(), srv.router.SlowFactor(); w != 0.5 || f != 2 {
		t.Fatalf("applied tokenWeight %v slowFactor %v, want 0.5 and 2", w, f)
	}
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "off"})
	if w, f := srv.router.TokenWeight(), srv.router.SlowFactor(); w != 0 || f != 0 {
		t.Fatalf("an absent field left tokenWeight %v slowFactor %v", w, f)
	}
}

// A worker the router ranks down is named on /loadz and recorded on the event
// stream as worker.slow, and its clear as worker.slow_cleared (T28.12).
func TestASlowWorkerReachesLoadzAndTheEventStream(t *testing.T) {
	srv, st := slowTestServer(t)
	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "on", Routing: PolicyRouting{SlowFactor: 2}})
	ctx := context.Background()
	worker := func(id string, firstEvent time.Duration) {
		w := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
			rw.Header().Set("Content-Type", "text/event-stream")
			time.Sleep(firstEvent)
			_, _ = io.WriteString(rw, `data: {"choices":[{"index":0,"delta":{"content":"a"}}]}`+"\n\n")
			_, _ = io.WriteString(rw, `data: {"choices":[{"index":0,"delta":{"content":"b"}}]}`+"\n\n")
			_, _ = io.WriteString(rw, "data: [DONE]\n\n")
		}))
		t.Cleanup(w.Close)
		if err := st.Nodes().Upsert(ctx, store.Node{ID: id, State: store.NodeStateReady, Address: w.Listener.Addr().String(), LastHeartbeat: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := st.Placements().Upsert(ctx, store.Placement{NodeID: id, ModelID: "m", Status: "ready", LastSeen: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	worker("slow", 30*time.Millisecond)
	worker("fast", time.Millisecond)
	// Twenty answers from each: the other worker is drained meanwhile, so
	// every request lands on the one being measured.
	for _, w := range [][2]string{{"slow", "fast"}, {"fast", "slow"}} {
		if _, err := st.Nodes().SetState(ctx, w[1], store.NodeStateDraining); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Nodes().SetState(ctx, w[0], store.NodeStateReady); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			s, err := srv.router.Chat(ctx, engines.ChatRequest{Model: "m", Stream: true,
				Messages: []engines.Message{{Role: "user", Content: "hello"}}})
			if err != nil {
				t.Fatal(err)
			}
			for range s {
			}
		}
	}
	deadline := time.Now().Add(2 * time.Second) // the last sample is recorded after the stream closes
	for len(srv.router.SlowWorkers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	rec := httptest.NewRecorder()
	srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
	var load adminapi.Load
	if err := json.Unmarshal(rec.Body.Bytes(), &load); err != nil {
		t.Fatal(err)
	}
	if len(load.SlowWorkers) != 1 || load.SlowWorkers[0].Worker != "slow" || load.SlowWorkers[0].Model != "m" ||
		load.SlowWorkers[0].Factor <= 2 || load.SlowWorkers[0].Samples != 20 {
		t.Fatalf("/loadz slow_workers %+v", load.SlowWorkers)
	}

	srv.applyPolicySnapshot(&PolicySnapshot{Revision: "off"}) // clears it
	rows, err := st.EventLog().After(context.Background(), 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	for _, e := range rows {
		if e.Subject == "slow" {
			types = append(types, e.Type)
		}
	}
	if len(types) != 2 || types[0] != eventWorkerSlow || types[1] != eventWorkerSlowCleared {
		t.Fatalf("events for the slow worker %v, want worker.slow then worker.slow_cleared", types)
	}
	if got := topicFor(eventWorkerSlow); got != topicFor("node.registered") {
		t.Fatalf("worker.slow rides topic %s, want the nodes topic", got)
	}
	rec = httptest.NewRecorder()
	srv.loadz(rec, httptest.NewRequest(http.MethodGet, "/loadz", nil))
	if json.Unmarshal(rec.Body.Bytes(), &load) != nil {
		t.Fatal("bad /loadz")
	}
	var raw map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &raw)
	if _, present := raw["slow_workers"]; present {
		t.Fatalf("slow_workers present with no slow worker: %v", raw["slow_workers"])
	}
}

func slowTestServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return NewServer(cfg, st, &stubLeaderEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil), st
}
