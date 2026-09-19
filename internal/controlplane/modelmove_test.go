package controlplane

// POST /admin/v1/models/{id}/move (feature model_move), proven through the
// real gateway, router and heartbeat path against fake workers: no request
// fails during a move, no new request reaches the source after the flip, a
// target that never serves leaves the source serving and is cleaned up, and
// every refusal is made before anything is touched.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/auth"
	"github.com/opod-io/opod/internal/config"
	_ "github.com/opod-io/opod/internal/engines/all" // the drivers' one-model-per-process facts
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/scheduler"
	"github.com/opod-io/opod/internal/store"
)

// moveWorker is a worker as the leader sees one: the OpenAI surface, the load
// and unload routes, and a heartbeat the fixture sends for it every few ms
// from what it currently lists.
type moveWorker struct {
	*httptest.Server
	id     string
	engine string

	mu          sync.Mutex
	listed      []string // loaded_models
	resident    []string // resident_models; sent only when keepsWeights
	neverServes bool     // the load is accepted and the model never listed
	unloadTakes time.Duration
	unloadFails bool // the worker answers 409: something of the model is held there
	chatStarts  []time.Time
	loads       int
	unloads     int
	chats       atomic.Int64
}

// keepsWeights: an engine that keeps installed models and reports residency.
func (w *moveWorker) keepsWeights() bool { return w.engine == "ollama" }

func without(list []string, id string) []string {
	out := list[:0:0]
	for _, m := range list {
		if m != id {
			out = append(out, m)
		}
	}
	return out
}

func newMoveWorker(t *testing.T, id, engine string, serving ...string) *moveWorker {
	t.Helper()
	w := &moveWorker{id: id, engine: engine, listed: serving, resident: append([]string(nil), serving...)}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		var body struct {
			ID string `json:"id"`
		}
		switch r.URL.Path {
		case "/v1/chat/completions":
			w.mu.Lock()
			w.chatStarts = append(w.chatStarts, time.Now())
			w.mu.Unlock()
			w.chats.Add(1)
			time.Sleep(3 * time.Millisecond) // a request is in flight for a moment
			rw.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(rw, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
			fmt.Fprint(rw, "data: [DONE]\n\n")
		case "/v1/model/load":
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.mu.Lock()
			w.loads++
			if !w.neverServes {
				w.listed = append(without(w.listed, body.ID), body.ID)
				w.resident = append(without(w.resident, body.ID), body.ID)
			}
			w.mu.Unlock()
			_, _ = io.WriteString(rw, `{"status":"ready"}`)
		case "/v1/model/unload":
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.mu.Lock()
			takes, fails := w.unloadTakes, w.unloadFails
			if fails {
				w.unloads++
			}
			w.mu.Unlock()
			if fails {
				http.Error(rw, "model \"m\" still holds adapter(s) m:late on this worker", http.StatusConflict)
				return
			}
			time.Sleep(takes) // stopping an engine is not instant; it still lists the model meanwhile
			w.mu.Lock()
			w.unloads++
			w.resident = without(w.resident, body.ID)
			if !w.keepsWeights() {
				w.listed = without(w.listed, body.ID)
			}
			w.mu.Unlock()
			_, _ = io.WriteString(rw, `{"status":"unloaded","model":"`+body.ID+`"}`)
		default:
			http.NotFound(rw, r)
		}
	}))
	t.Cleanup(w.Close)
	return w
}

func (w *moveWorker) lastChatStart() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.chatStarts) == 0 {
		return time.Time{}
	}
	return w.chatStarts[len(w.chatStarts)-1]
}

// moveFixture: a router-only leader with an orchestrator and a catalog that
// knows "m" (8 GB), in front of the given workers, each heartbeating for real.
func moveFixture(t *testing.T, workers ...*moveWorker) (*Server, *httptest.Server) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.Auth.RequireKeys = false
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	plain, rec, err := auth.Generate("move-test-admin", "admin", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.APIKeys().Create(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	drainAdminKey = plain
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	cat := []models.Entry{
		{ID: "m", SizeBytes: 8 << 30, Source: models.SourceSpec{Type: "huggingface", Repo: "example/m"}},
		{ID: "gang", SizeBytes: 8 << 30, Source: models.SourceSpec{Type: "huggingface", Repo: "example/gang"}, Sharding: models.ShardingSpec{Required: true}},
		{ID: "split", SizeBytes: 1 << 30},  // served by a gang in the refusal test
		{ID: "absent", SizeBytes: 1 << 30}, // held by nobody
	}
	srv := NewServer(cfg, st, &downEngine{}, cat, log, scheduler.New(st, nil, log, t.TempDir()))
	srv.movePoll, srv.moveSettle = 5*time.Millisecond, 500*time.Millisecond
	ts := httptest.NewServer(srv.routes())
	t.Cleanup(ts.Close)

	ctx, stop := context.WithCancel(context.Background())
	var beats sync.WaitGroup
	t.Cleanup(func() { stop(); beats.Wait() })
	for _, w := range workers {
		hw, _ := json.Marshal(agent.Capabilities{Hostname: w.id, OS: "linux", Arch: "amd64", RAMGB: 64, Engine: w.engine})
		if _, err := srv.RegisterNode(ctx, RegisterRequest{ID: w.id, Hostname: w.id, RAMGB: 64, Address: w.URL, HardwareJSON: string(hw)}, Caller{Admin: true}, "tok"); err != nil {
			t.Fatal(err)
		}
		beat := func(w *moveWorker) {
			w.mu.Lock()
			req := HeartbeatRequest{ID: w.id, LoadedModels: append([]string(nil), w.listed...)}
			if w.keepsWeights() {
				resident := append([]string{}, w.resident...)
				req.ResidentModels = &resident
			}
			w.mu.Unlock()
			_ = srv.HeartbeatNode(ctx, req, Caller{Admin: true})
		}
		beat(w)
		beats.Add(1)
		go func(w *moveWorker) {
			defer beats.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Millisecond):
					beat(w)
				}
			}
		}(w)
	}
	return srv, ts
}

type moveAnswer struct {
	Steps []scheduler.MoveStep `json:"steps"`
	Note  string               `json:"note"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (a moveAnswer) step(name string) (scheduler.MoveStep, bool) {
	for _, s := range a.Steps {
		if s.Step == name {
			return s, true
		}
	}
	return scheduler.MoveStep{}, false
}

func postMove(t *testing.T, ts *httptest.Server, model, body string) (int, moveAnswer, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/v1/models/"+model+"/move", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+drainAdminKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var ans moveAnswer
	_ = json.Unmarshal(raw, &ans)
	return resp.StatusCode, ans, string(raw)
}

// traffic runs clients against the gateway until stopped and reports how many
// requests were made and every one that did not answer 200.
func traffic(ts *httptest.Server, clients int) (stop func() (total int64, failures []string)) {
	var (
		wg    sync.WaitGroup
		total atomic.Int64
		mu    sync.Mutex
		bad   []string
		done  = make(chan struct{})
	)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(drainChatBody)))
				total.Add(1)
				if err != nil {
					mu.Lock()
					bad = append(bad, err.Error())
					mu.Unlock()
					continue
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					mu.Lock()
					bad = append(bad, fmt.Sprintf("%d %s", resp.StatusCode, b))
					mu.Unlock()
				}
			}
		}()
	}
	return func() (int64, []string) {
		close(done)
		wg.Wait()
		return total.Load(), bad
	}
}

func rowStatus(t *testing.T, srv *Server, node, model string) string {
	t.Helper()
	rows, err := srv.store.Placements().GetByNode(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ModelID == model {
			return r.Status
		}
	}
	return ""
}

// The move itself: requests run the whole time and none fails; once the
// source is flipped no new request reaches it — although it keeps listing the
// model in its heartbeats while its engine stops — and at the end only the
// target serves. Every step is journalled.
func TestModelMoveFailsNoRequestAndFlipsTheSource(t *testing.T) {
	w1 := newMoveWorker(t, "w1", "vllm", "m")
	w2 := newMoveWorker(t, "w2", "vllm")
	w1.unloadTakes = 300 * time.Millisecond
	srv, ts := moveFixture(t, w1, w2)

	stop := traffic(ts, 4)
	time.Sleep(50 * time.Millisecond) // the source is serving under load before the move starts
	code, ans, raw := postMove(t, ts, "m", `{"from":"w1","to":"w2"}`)
	time.Sleep(50 * time.Millisecond) // and the target after it
	total, failures := stop()

	if code != http.StatusOK {
		t.Fatalf("move: %d %s", code, raw)
	}
	if len(failures) > 0 {
		t.Fatalf("%d of %d requests failed during the move, first: %s", len(failures), total, failures[0])
	}
	if total < 20 {
		t.Fatalf("only %d requests ran during the move — the proof needs traffic", total)
	}
	var order []string
	for _, s := range ans.Steps {
		order = append(order, s.Step)
	}
	if strings.Join(order, ",") != "started,loaded,flipped,drained,finished" {
		t.Fatalf("steps: %v", order)
	}
	flipped, _ := ans.step(scheduler.MoveFlipped)
	finished, _ := ans.step(scheduler.MoveFinished)
	if finished.At.Sub(flipped.At) < 250*time.Millisecond {
		t.Fatalf("the source kept listing the model for only %s after the flip; the window this test watches is too short", finished.At.Sub(flipped.At))
	}
	// A request picked a moment before the flip may start a moment after it;
	// nothing may start on the source once that moment has passed, although
	// the source heartbeats the model for another ~300 ms.
	if last := w1.lastChatStart(); last.After(flipped.At.Add(100 * time.Millisecond)) {
		t.Errorf("a request reached the source %s after the flip", last.Sub(flipped.At))
	}
	if drained, _ := ans.step(scheduler.MoveDrained); drained.Data["timed_out"] != false {
		t.Errorf("the source must go idle, not time out: %v", drained.Data)
	}
	if w2.chats.Load() == 0 || w1.chats.Load() == 0 {
		t.Errorf("both workers must have served during the move: w1 %d, w2 %d", w1.chats.Load(), w2.chats.Load())
	}
	if w1.unloads != 1 || w2.loads != 1 || w2.unloads != 0 {
		t.Errorf("w1 unloads %d, w2 loads %d unloads %d; want 1, 1, 0", w1.unloads, w2.loads, w2.unloads)
	}
	time.Sleep(40 * time.Millisecond) // a few heartbeats
	if got := rowStatus(t, srv, "w1", "m"); got != "" {
		t.Errorf("the source's row must be gone, got %q", got)
	}
	if got := rowStatus(t, srv, "w2", "m"); got != "ready" {
		t.Errorf("the target's row: %q", got)
	}

	// The journal carries every step.
	evs, err := srv.store.EventLog().After(context.Background(), 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range evs {
		seen[e.Type] = true
	}
	for _, want := range []string{"model.move_started", "model.move_loaded", "model.move_flipped", "model.move_drained", "model.move_finished"} {
		if !seen[want] {
			t.Errorf("event %s was not journalled", want)
		}
	}
}

// The design's central safety property: a target that never serves the model
// leaves the source serving, untouched, and the target's copy is unloaded.
func TestModelMoveTargetNeverServesLeavesTheSourceServing(t *testing.T) {
	w1 := newMoveWorker(t, "w1", "vllm", "m")
	w2 := newMoveWorker(t, "w2", "vllm")
	w2.neverServes = true
	srv, ts := moveFixture(t, w1, w2)

	stop := traffic(ts, 2)
	code, ans, raw := postMove(t, ts, "m", `{"from":"w1","to":"w2","ready_timeout_seconds":1}`)
	total, failures := stop()

	if code != http.StatusBadGateway {
		t.Fatalf("an aborted move is a 502 with its steps: %d %s", code, raw)
	}
	aborted, ok := ans.step(scheduler.MoveAborted)
	if !ok || !strings.Contains(fmt.Sprint(aborted.Data["state"]), "the source still serves") ||
		!strings.Contains(fmt.Sprint(aborted.Data["state"]), "unloaded") {
		t.Fatalf("the aborted step must say what serves now: %+v", ans.Steps)
	}
	if _, flipped := ans.step(scheduler.MoveFlipped); flipped {
		t.Fatal("a move whose target never served must not flip the source")
	}
	if len(failures) > 0 || total == 0 {
		t.Fatalf("%d of %d requests failed while the move waited and aborted, first: %v", len(failures), total, failures)
	}
	if got := rowStatus(t, srv, "w1", "m"); got != "ready" {
		t.Errorf("the source's row must still be ready, got %q", got)
	}
	if w1.unloads != 0 {
		t.Error("the source must never be asked to unload")
	}
	if w2.loads != 1 || w2.unloads != 1 {
		t.Errorf("the target is loaded once and cleaned up once: loads %d, unloads %d", w2.loads, w2.unloads)
	}
	before := w1.chats.Load()
	if resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(drainChatBody)); err != nil || resp.StatusCode != 200 {
		t.Fatalf("the source must still answer: %v %v", err, resp)
	}
	if w1.chats.Load() != before+1 {
		t.Error("the request after the abort did not reach the source")
	}
}

// A source whose engine keeps the weights installed (it goes on listing the
// model after the unload): the placement is released — not routed to, across
// heartbeats and a leader start — and the answer says so.
func TestModelMoveOffAnEngineThatKeepsWeightsReleasesThePlacement(t *testing.T) {
	w1 := newMoveWorker(t, "w1", "ollama", "m")
	w2 := newMoveWorker(t, "w2", "ollama")
	srv, ts := moveFixture(t, w1, w2)

	code, ans, raw := postMove(t, ts, "m", `{"from":"w1","to":"w2"}`)
	if code != http.StatusOK {
		t.Fatalf("move: %d %s", code, raw)
	}
	if !strings.Contains(ans.Note, "keeps the weights") || !strings.Contains(ans.Note, "released") {
		t.Errorf("the answer must say the source keeps the weights: %q", ans.Note)
	}
	time.Sleep(40 * time.Millisecond) // heartbeats that still list the model
	if got := rowStatus(t, srv, "w1", "m"); got != store.PlacementReleased {
		t.Fatalf("the source's row: %q, want released", got)
	}
	srv.liftStaleDrains(context.Background()) // what a leader start does
	if got := rowStatus(t, srv, "w1", "m"); got != store.PlacementReleased {
		t.Fatalf("a leader start must not put a released placement back: %q", got)
	}
	before := w1.chats.Load()
	for i := 0; i < 10; i++ {
		if resp, err := http.Post(ts.URL+"/v1/chat/completions", "application/json", strings.NewReader(drainChatBody)); err != nil || resp.StatusCode != 200 {
			t.Fatalf("request %d after the move: %v %v", i, err, resp)
		}
	}
	if w1.chats.Load() != before {
		t.Errorf("%d request(s) reached the released source", w1.chats.Load()-before)
	}
}

// Every refusal is made before anything is touched, with the reason.
func TestModelMoveRefusals(t *testing.T) {
	w1 := newMoveWorker(t, "w1", "vllm", "m")
	w2 := newMoveWorker(t, "w2", "vllm", "other")    // a one-model engine serving something else
	w3 := newMoveWorker(t, "w3", "vllm")             // too small, below
	w4 := newMoveWorker(t, "w4", "vllm")             // drained, below
	w5 := newMoveWorker(t, "w5", "vllm", "m:legal")  // nothing to do with the move; holds an adapter row only
	w6 := newMoveWorker(t, "w6", "vllm", "m", "m:x") // a source with an adapter of the model
	srv, ts := moveFixture(t, w1, w2, w3, w4, w5, w6)
	ctx := context.Background()

	small, _ := srv.store.Nodes().Get(ctx, "w3")
	small.RAMGB = 4
	hw, _ := json.Marshal(agent.Capabilities{Hostname: "w3", RAMGB: 4, Engine: "vllm"})
	small.HardwareJSON = string(hw)
	if err := srv.store.Nodes().Upsert(ctx, *small); err != nil {
		t.Fatal(err)
	}
	if err := srv.DrainNode(ctx, "w4"); err != nil {
		t.Fatal(err)
	}
	if err := srv.store.Shards().Create(ctx, store.Shard{ID: "s-split-rpc-0", ModelID: "split", Role: "rpc", NodeID: "w1", Status: "ready"}); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name, model, body string
		code              int
		want              []string
	}{
		{"no body", "m", `{}`, http.StatusBadRequest, []string{"from and to are required"}},
		{"unknown model", "nope", `{"from":"w1","to":"w3"}`, http.StatusNotFound, []string{"no catalog entry"}},
		{"unknown source", "m", `{"from":"ghost","to":"w3"}`, http.StatusNotFound, []string{"ghost", "--from"}},
		{"unknown target", "m", `{"from":"w1","to":"ghost"}`, http.StatusNotFound, []string{"ghost", "--to"}},
		{"same node", "m", `{"from":"w1","to":"w1"}`, http.StatusConflict, []string{"same node"}},
		{"source does not hold it", "absent", `{"from":"w1","to":"w5"}`, http.StatusConflict, []string{"w1 does not hold absent"}},
		{"target serves something else on a one-model engine", "m", `{"from":"w1","to":"w2"}`, http.StatusConflict, []string{"one model per process", "other", "m"}},
		{"target too small", "m", `{"from":"w1","to":"w3"}`, http.StatusConflict, []string{"cannot hold m", "needs", "free"}},
		{"target does not take new work", "m", `{"from":"w1","to":"w4"}`, http.StatusConflict, []string{"does not take new work", "draining"}},
		{"a catalog entry that must be sharded", "gang", `{"from":"w1","to":"w5"}`, http.StatusConflict, []string{"sharded", "opod shard create"}},
		{"a model served by a gang", "split", `{"from":"w1","to":"w5"}`, http.StatusConflict, []string{"sharded placement"}},
		{"the source serves an adapter of it", "m", `{"from":"w6","to":"w5"}`, http.StatusConflict, []string{"adapter", "m:x"}},
	} {
		code, ans, raw := postMove(t, ts, c.model, c.body)
		if code != c.code {
			t.Errorf("%s: %d %s, want %d", c.name, code, raw, c.code)
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(ans.Error.Message, w) {
				t.Errorf("%s: %q does not mention %q", c.name, ans.Error.Message, w)
			}
		}
	}
	for _, w := range []*moveWorker{w1, w2, w3, w4, w5, w6} {
		if w.loads != 0 || w.unloads != 0 {
			t.Errorf("%s: a refused move touched a worker (loads %d, unloads %d)", w.id, w.loads, w.unloads)
		}
	}
	if got := rowStatus(t, srv, "w1", "m"); got != "ready" {
		t.Errorf("the source's row after the refusals: %q", got)
	}
}

// A source that cannot let the model go after the flip is put back in
// rotation — two workers serving is true and harmless, a draining row over a
// model nobody will unload is neither — and the answer says so.
func TestModelMoveSourceThatCannotUnloadGoesBackInRotation(t *testing.T) {
	w1 := newMoveWorker(t, "w1", "vllm", "m")
	w2 := newMoveWorker(t, "w2", "vllm")
	w1.unloadFails = true
	srv, ts := moveFixture(t, w1, w2)

	code, ans, raw := postMove(t, ts, "m", `{"from":"w1","to":"w2"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("move: %d %s", code, raw)
	}
	aborted, ok := ans.step(scheduler.MoveAborted)
	if !ok || !strings.Contains(fmt.Sprint(aborted.Data["state"]), "both workers serve") || !strings.Contains(fmt.Sprint(aborted.Data["reason"]), "m:late") {
		t.Fatalf("the aborted step: %+v", ans.Steps)
	}
	if got := rowStatus(t, srv, "w1", "m"); got != "ready" {
		t.Errorf("the source's row: %q, want ready again", got)
	}
	if got := rowStatus(t, srv, "w2", "m"); got != "ready" {
		t.Errorf("the target keeps serving: %q", got)
	}
	if w2.unloads != 0 {
		t.Error("the target must not be unloaded once it serves and the source was flipped")
	}
}

// A target that already serves the model is not loaded again — on a one-model
// engine a load would restart the very process that serves it.
func TestModelMoveOntoAReplicaLoadsNothing(t *testing.T) {
	w1 := newMoveWorker(t, "w1", "vllm", "m")
	w2 := newMoveWorker(t, "w2", "vllm", "m")
	srv, ts := moveFixture(t, w1, w2)

	code, ans, raw := postMove(t, ts, "m", `{"from":"w1","to":"w2"}`)
	if code != http.StatusOK {
		t.Fatalf("move: %d %s", code, raw)
	}
	if started, _ := ans.step(scheduler.MoveStarted); started.Data["target_already_serves"] != true {
		t.Errorf("started: %v", started.Data)
	}
	if w2.loads != 0 || w1.unloads != 1 {
		t.Errorf("w2 loads %d, w1 unloads %d; want 0 and 1", w2.loads, w1.unloads)
	}
	time.Sleep(40 * time.Millisecond)
	if got := rowStatus(t, srv, "w1", "m"); got != "" {
		t.Errorf("the source's row must be gone, got %q", got)
	}
}
