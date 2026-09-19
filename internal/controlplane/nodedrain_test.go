package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/store"
)

// fakeWorker is a worker's OpenAI surface: it counts the chats it is given
// and answers each with one streamed token.
type fakeWorker struct {
	*httptest.Server
	chats atomic.Int64
}

func newFakeWorker(t *testing.T) *fakeWorker {
	t.Helper()
	w := &fakeWorker{}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(rw, r)
			return
		}
		w.chats.Add(1)
		rw.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(rw, "data: [DONE]\n\n")
	}))
	t.Cleanup(w.Close)
	return w
}

// drainAdminKey is minted per fixture: the admin surface is always keyed.
var drainAdminKey string

// drainFixture: a router-only leader (its own engine is down) in front of two
// live workers that both hold model "m".
func drainFixture(t *testing.T) (*Server, *httptest.Server, map[string]*fakeWorker) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = false
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	plain, rec, err := auth.Generate("drain-test-admin", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.APIKeys().Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	drainAdminKey = plain
	srv := NewServer(cfg, st, &downEngine{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)

	workers := map[string]*fakeWorker{"w1": newFakeWorker(t), "w2": newFakeWorker(t)}
	ctx := context.Background()
	for id, w := range workers {
		if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: id, Hostname: id, Address: w.URL}, Caller{Admin: true}, "tok"); err != nil {
			t.Fatal(err)
		}
		if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: id, LoadedModels: []string{"m"}}, Caller{Admin: true}); err != nil {
			t.Fatal(err)
		}
	}
	return srv, ts, workers
}

func adminPost(t *testing.T, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+drainAdminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const drainChatBody = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

// TestDrainedNodeGetsNoNewRequests: the whole promise of `opod node drain`.
// With two workers, draining one sends every new request to the other and
// none fails; draining the last one answers the waking 503 with Retry-After
// (the path an unavailable model already takes); undrain puts it back.
func TestDrainedNodeGetsNoNewRequests(t *testing.T) {
	srv, ts, workers := drainFixture(t)
	ctx := context.Background()

	// Before: both are chosen (least in-flight, ties broken by order — all
	// that matters here is that w1 is not starved for a reason other than
	// the drain).
	for i := 0; i < 8; i++ {
		if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("baseline chat %d: %d %s", i, resp.StatusCode, out)
		}
	}
	if workers["w1"].chats.Load()+workers["w2"].chats.Load() != 8 {
		t.Fatalf("baseline: 8 chats, workers saw %d + %d", workers["w1"].chats.Load(), workers["w2"].chats.Load())
	}

	if code, out := adminPost(t, ts, "/admin/v1/nodes/w1/drain"); code != http.StatusOK || !strings.Contains(out, `"draining"`) {
		t.Fatalf("drain: %d %s", code, out)
	}
	// Heartbeats keep arriving from a draining worker; they must not undo it.
	for i := 0; i < 3; i++ {
		if err := srv.HeartbeatNode(ctx, HeartbeatRequest{ID: "w1", LoadedModels: []string{"m"}}, Caller{Admin: true}); err != nil {
			t.Fatal(err)
		}
	}
	before1, before2 := workers["w1"].chats.Load(), workers["w2"].chats.Load()
	const n = 20
	for i := 0; i < n; i++ {
		if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("chat %d with w1 draining failed: %d %s", i, resp.StatusCode, out)
		}
	}
	if got := workers["w1"].chats.Load() - before1; got != 0 {
		t.Errorf("the draining worker was given %d new requests, want 0", got)
	}
	if got := workers["w2"].chats.Load() - before2; got != n {
		t.Errorf("the other worker served %d of %d", got, n)
	}

	// The listing says what the node is.
	rec := httptest.NewRecorder()
	srv.listNodes(rec, httptest.NewRequest(http.MethodGet, "/admin/v1/nodes", nil))
	var rows []map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &rows)
	for _, r := range rows {
		if r["ID"] == "w1" && r["state"] != store.NodeStateDraining {
			t.Errorf("listing reports w1 as %v, want draining", r["state"])
		}
	}

	// The only worker left drains too: nothing can take a new request.
	if code, out := adminPost(t, ts, "/admin/v1/nodes/w2/drain"); code != http.StatusOK {
		t.Fatalf("drain w2: %d %s", code, out)
	}
	resp, out := chat(t, ts, drainChatBody, "")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("every worker draining: want 503 + Retry-After, got %d (Retry-After %q) %s", resp.StatusCode, resp.Header.Get("Retry-After"), out)
	}
	if srv.hasServingCapacity(ctx) {
		t.Error("draining workers must not count as serving capacity")
	}

	// Undrain: chosen again, from the next request on.
	if code, out := adminPost(t, ts, "/admin/v1/nodes/w1/undrain"); code != http.StatusOK || !strings.Contains(out, `"ready"`) {
		t.Fatalf("undrain: %d %s", code, out)
	}
	before1 = workers["w1"].chats.Load()
	if resp, out := chat(t, ts, drainChatBody, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("chat after undrain: %d %s", resp.StatusCode, out)
	}
	if workers["w1"].chats.Load() != before1+1 {
		t.Error("the undrained worker was not chosen again")
	}

	if code, _ := adminPost(t, ts, "/admin/v1/nodes/nobody/undrain"); code != http.StatusNotFound {
		t.Errorf("undrain of an unknown node = %d, want 404", code)
	}
	if code, _ := adminPost(t, ts, "/admin/v1/nodes/nobody/drain"); code != http.StatusNotFound {
		t.Errorf("drain of an unknown node = %d, want 404", code)
	}
}

// TestDrainSurvivesReRegister: a drained worker that restarts and registers
// again (new boot id) is still drained — the drain is about the node, and
// only undrain ends it.
func TestDrainSurvivesReRegister(t *testing.T) {
	srv, _, workers := drainFixture(t)
	ctx := context.Background()
	if err := srv.DrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: "w1", Hostname: "w1", Address: workers["w1"].URL, BootID: "boot-2"}, Caller{Admin: true}, "tok"); err != nil {
		t.Fatal(err)
	}
	if n, _ := srv.store.Nodes().Get(ctx, "w1"); n == nil || !n.Draining() {
		t.Fatalf("after a re-register the node reads %+v, want draining", n)
	}
	if err := srv.UndrainNode(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := srv.store.Nodes().Get(ctx, "w1"); n == nil || n.State != store.NodeStateReady {
		t.Fatalf("after undrain the node reads %+v, want ready", n)
	}
}

// TestDrainTakesAGangOut: a sharded model is one serving unit, so a drain of
// the node under ANY of its parts stops new requests to the gang — also after
// the router cached the coordinator's engine on an earlier request.
func TestDrainTakesAGangOut(t *testing.T) {
	srv, ts, workers := drainFixture(t)
	ctx := context.Background()
	st := srv.store
	// "g" is served by a gang: coordinator on w1, an rpc part on w2. No
	// placement rows for it — a gang has none on workers.
	coord := strings.TrimPrefix(workers["w1"].URL, "http://")
	for _, sh := range []store.Shard{
		{ID: "c", ModelID: "g", Role: "coordinator", NodeID: "w1", Address: coord, Status: "ready", ConfigJSON: `{"engine":"vllm"}`, CreatedAt: time.Now(), LastSeen: time.Now()},
		{ID: "p", ModelID: "g", Role: "rpc", NodeID: "w2", Address: "192.0.2.10:50052", Status: "ready", CreatedAt: time.Now(), LastSeen: time.Now()},
	} {
		if err := st.Shards().Create(ctx, sh); err != nil {
			t.Fatal(err)
		}
	}
	// The plain model's rows go, so the gang is the only capacity.
	for id := range workers {
		if err := st.Placements().ReplaceForNode(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"model":"g","messages":[{"role":"user","content":"hi"}]}`
	if resp, out := chat(t, ts, body, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("gang chat: %d %s", resp.StatusCode, out)
	}
	served := workers["w1"].chats.Load()

	if err := srv.DrainNode(ctx, "w2"); err != nil { // the rpc part's node, not the coordinator's
		t.Fatal(err)
	}
	resp, out := chat(t, ts, body, "")
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("gang with a part on a draining node: want 503 + Retry-After, got %d %s", resp.StatusCode, out)
	}
	if workers["w1"].chats.Load() != served {
		t.Error("the coordinator was given a request while a part's node drains")
	}
	if err := srv.UndrainNode(ctx, "w2"); err != nil {
		t.Fatal(err)
	}
	if resp, out := chat(t, ts, body, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("gang chat after undrain: %d %s", resp.StatusCode, out)
	}
}

// TestLeaderNodeIsNotDrainable: the router serves from the local engine
// before it looks at any worker, so a drain of "local" would be a state
// nothing honours — refused instead of pretended.
func TestLeaderNodeIsNotDrainable(t *testing.T) {
	srv, ts, _ := drainFixture(t)
	if err := srv.store.Nodes().Upsert(context.Background(), store.Node{ID: "local", Hostname: "leader", State: store.NodeStateReady, LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if code, out := adminPost(t, ts, "/admin/v1/nodes/local/drain"); code != http.StatusBadRequest {
		t.Fatalf("drain local: %d %s", code, out)
	}
	if n, _ := srv.store.Nodes().Get(context.Background(), "local"); n == nil || n.Draining() {
		t.Fatalf("local reads %+v after a refused drain", n)
	}
}
