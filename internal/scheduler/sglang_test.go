package scheduler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/agent"
	"github.com/opod-io/opod/internal/models"
	"github.com/opod-io/opod/internal/store"
)

// startedProcs is a worker that records what it was asked to run, so a test can
// read the actual command line a machine would have executed.
type startedProcs struct {
	mu      sync.Mutex
	specs   []agent.ProcessSpec
	stopped []string
	failAt  int // 1-based index of the start that fails; 0 = never
	srv     *httptest.Server
}

func newFakeWorker(t *testing.T, failAt int) *startedProcs {
	t.Helper()
	w := &startedProcs{failAt: failAt}
	w.srv = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/process/start"):
			var spec agent.ProcessSpec
			if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
				t.Errorf("process spec: %v", err)
			}
			if w.failAt > 0 && len(w.specs)+1 == w.failAt {
				http.Error(rw, "no room on this machine", http.StatusInternalServerError)
				return
			}
			w.specs = append(w.specs, spec)
			_ = json.NewEncoder(rw).Encode(map[string]any{"id": spec.ID, "status": "running"})
		case strings.HasSuffix(r.URL.Path, "/v1/process/stop"):
			var body struct {
				ID string `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.ID == "" { // some callers put it in the path
				parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
				body.ID = parts[len(parts)-1]
			}
			w.stopped = append(w.stopped, body.ID)
			_, _ = io.WriteString(rw, `{}`)
		default:
			_, _ = io.WriteString(rw, `[]`) // the process LIST the orphan sweep asks for
		}
	}))
	t.Cleanup(w.srv.Close)
	return w
}

func (w *startedProcs) commands() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]string, 0, len(w.specs))
	for _, s := range w.specs {
		out = append(out, strings.Join(s.Args, " "))
	}
	return out
}

func sglangFixture(t *testing.T, workers ...*startedProcs) (*Orchestrator, models.Entry, []store.Node) {
	t.Helper()
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	var nodes []store.Node
	for i, w := range workers {
		n := store.Node{
			ID: string(rune('a' + i)), Address: strings.TrimPrefix(w.srv.URL, "http://"),
			WorkerToken: "tok", State: store.NodeStateReady, LastHeartbeat: time.Now(), RAMGB: 64,
		}
		if err := st.Nodes().Upsert(ctx, n); err != nil {
			t.Fatal(err)
		}
		nodes = append(nodes, n)
	}
	o := New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), t.TempDir())
	entry := models.Entry{
		ID:       "big-model",
		Source:   models.SourceSpec{Type: "huggingface", Repo: "vendor/Big-Model"},
		Sharding: models.ShardingSpec{Required: true, Engine: "sglang", DefaultShards: 2},
	}
	return o, entry, nodes
}

// The whole point of the backend: SGLang is started ONCE PER RANK with the same
// rendezvous address and a different --node-rank, and the tensor width is the
// gang's, not one machine's. Anything else forms no group.
func TestSGLangGangStartsOneRankPerMachine(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, entry, nodes := sglangFixture(t, w0, w1)
	ctx := context.Background()

	if err := o.CreateSharded(ctx, entry, "g0", 0, []string{nodes[0].ID, nodes[1].ID}, Parallelism{}); err != nil {
		t.Fatalf("create: %v", err)
	}

	cmds := append(w0.commands(), w1.commands()...)
	if len(cmds) != 2 {
		t.Fatalf("a two-machine gang must start two ranks, got %d: %v", len(cmds), cmds)
	}
	var ranks []string
	for _, c := range cmds {
		if !strings.Contains(c, "sglang.launch_server") {
			t.Errorf("a rank must run SGLang's own launcher: %q", c)
		}
		// One rendezvous for the whole gang: it is rank 0's address, and every
		// rank — including rank 0 — dials the same one.
		if !strings.Contains(c, "--dist-init-addr "+hostOf(nodes[0].Address)+":") {
			t.Errorf("every rank dials rank 0's rendezvous: %q", c)
		}
		if !strings.Contains(c, "--nnodes 2") {
			t.Errorf("--nnodes must be the gang's size: %q", c)
		}
		// The default split over two single-card parts is tp=1 pp=2 (TP inside a
		// part, PP across them) — the safe shape, and the one that does not put
		// an all-reduce on the wire.
		if !strings.Contains(c, "--tp-size 1") || !strings.Contains(c, "--pp-size 2") {
			t.Errorf("default shape over two parts is tp=1 pp=2: %q", c)
		}
		if !strings.Contains(c, "--host 0.0.0.0") {
			t.Errorf("a rank the leader must dial cannot bind the loopback: %q", c)
		}
		for _, f := range []string{"--node-rank 0", "--node-rank 1"} {
			if strings.Contains(c, f) {
				ranks = append(ranks, f)
			}
		}
	}
	if len(ranks) != 2 || ranks[0] == ranks[1] {
		t.Errorf("each machine must be told a DIFFERENT rank, got %v", ranks)
	}

	shards, err := o.Store.Shards().GetByGang(ctx, entry.ID, "g0")
	if err != nil {
		t.Fatal(err)
	}
	var coords, parts int
	for _, s := range shards {
		switch s.Role {
		case "coordinator":
			coords++
			if !strings.Contains(s.ConfigJSON, `"sglang"`) {
				t.Errorf("the router picks a coordinator's driver from its config: %q", s.ConfigJSON)
			}
			if !strings.Contains(s.Address, ":") {
				t.Errorf("a coordinator the router dials needs a port: %q", s.Address)
			}
		case "rank":
			parts++
		}
	}
	// Rank 0 IS the coordinator: one process, one row. A second row over the
	// same process id would have every teardown stop it twice.
	if coords != 1 || parts != 1 {
		t.Errorf("a two-rank gang records one coordinator and one rank, got %d and %d", coords, parts)
	}
	if p, err := o.Store.Placements().GetByModel(ctx, entry.ID); err != nil || len(p) == 0 {
		t.Errorf("the router reads a placement; none was written (%v)", err)
	}
}

// A tensor group wider than one machine is exactly what D8 forbade and ADR-068
// allows. It must reach the engine as ONE number over the whole gang — a
// per-machine TP would silently serve a different model shape.
func TestSGLangTensorWidthIsTheGangs(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, entry, nodes := sglangFixture(t, w0, w1)

	if err := o.CreateSharded(context.Background(), entry, "g0", 0, []string{nodes[0].ID, nodes[1].ID}, Parallelism{TP: 2}); err != nil {
		t.Fatalf("tensor parallel across nodes must be ACCEPTED since ADR-068: %v", err)
	}
	for _, c := range append(w0.commands(), w1.commands()...) {
		if !strings.Contains(c, "--tp-size 2") {
			t.Errorf("--tp-size is the gang's total width: %q", c)
		}
		// pp=1 is the absence of a pipeline, and --pp-size is version-bound in
		// SGLang: writing it where it changes nothing makes the gang depend on
		// a flag an older build would refuse at startup.
		if strings.Contains(c, "--pp-size") {
			t.Errorf("a gang with one pipeline stage must not write --pp-size at all: %q", c)
		}
	}
}

// A create that fails half way must leave NO rank running — and the rank that
// matters here is rank 1, whose row is a "rank". This is the defect the rollback
// switch carried: it knew "coordinator" and "rpc" parts and not "rank" ones, so
// a failed vLLM-Ray or SGLang create left every joined rank behind. Rank 0 alone
// would not have shown it — it is recorded as the coordinator, which the switch
// always handled.
func TestSGLangRollbackStopsEveryRankAlreadyStarted(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	w2 := newFakeWorker(t, 1) // the third machine refuses its first start
	o, entry, nodes := sglangFixture(t, w0, w1, w2)
	ctx := context.Background()

	err := o.CreateSharded(ctx, entry, "g0", 0, []string{nodes[0].ID, nodes[1].ID, nodes[2].ID}, Parallelism{})
	if err == nil {
		t.Fatal("a create whose third rank cannot start must fail")
	}
	for i, w := range []*startedProcs{w0, w1} {
		w.mu.Lock()
		started, stopped := len(w.specs), append([]string(nil), w.stopped...)
		w.mu.Unlock()
		if started != 1 {
			t.Fatalf("rank %d should have started once, got %d", i, started)
		}
		if len(stopped) != 1 {
			t.Errorf("rank %d started and was left RUNNING by the rollback (stopped=%v)", i, stopped)
		}
	}
	shards, err := o.Store.Shards().GetByGang(ctx, entry.ID, "g0")
	if err != nil {
		t.Fatal(err)
	}
	if len(shards) != 0 {
		t.Errorf("a rolled-back create leaves no shard rows, got %d", len(shards))
	}
}

// The command is data, so the flags can be read without a machine. Two gangs of
// one model on one host must not be handed the same ports either.
func TestSGLangCommandShape(t *testing.T) {
	got := sglangGangCommand(sglangGang{
		Model: "vendor/Big-Model", ServedAs: "big-model", ServePort: 30000,
		DistAddr: "10.0.0.1:29500", NNodes: 4, NodeRank: 3, TP: 4, PP: 2,
	})
	for _, want := range []string{
		"exec python3 -m sglang.launch_server",
		"--model-path 'vendor/Big-Model'",
		"--served-model-name 'big-model'",
		"--port 30000", "--tp-size 4", "--pp-size 2",
		"--dist-init-addr 10.0.0.1:29500", "--nnodes 4", "--node-rank 3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if !isSGLangBackend("sglang") || !isSGLangBackend(" SGLang ") || isSGLangBackend("vllm") {
		t.Error("the catalog's sharding.engine selects this backend by name")
	}
}

// A gang part gets the VRAM budget its PLAN gave it, not 85% of whatever card
// it landed on. The first cut hard-coded 0.85 and a part on an 8 GB card died
// allocating its attention workspace with 512 KiB free, on a plan that had
// given it 6 GB (design-partner cell, 2026-09-22) — a per-worker VRAM budget
// that one start path ignores is not a budget.
func TestSGLangGangHonoursThePartsVRAMBudget(t *testing.T) {
	got := sglangGangCommand(sglangGang{Model: "m", ServedAs: "m", ServePort: 30000,
		DistAddr: "10.0.0.1:29500", NNodes: 2, NodeRank: 1, TP: 1, PP: 2})
	if strings.Contains(got, "--mem-fraction-static 0.85") {
		t.Errorf("the fraction must come from OPOD_VRAM_BUDGET_GB, not a constant:\n%s", got)
	}
	if !strings.Contains(got, "OPOD_VRAM_BUDGET_GB") {
		t.Errorf("the budget line must run before the exec:\n%s", got)
	}
	if !strings.Contains(got, `--mem-fraction-static "$U"`) {
		t.Errorf("the flag must read the variable that line writes:\n%s", got)
	}
	// And it is the SAME rule the worker's own launches use — one rule, three
	// readers; a second copy is how the constant got here in the first place.
	if !strings.Contains(got, agent.MemFractionShell("0.85")) {
		t.Errorf("the gang must use agent.MemFractionShell, not its own arithmetic:\n%s", got)
	}
}
