package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/models"
)

// vllmEntry is the catalog entry a manager writes for a vLLM gang.
func vllmEntry() models.Entry {
	return models.Entry{
		ID:       "big-model",
		Source:   models.SourceSpec{Type: "huggingface", Repo: "vendor/Big-Model"},
		Sharding: models.ShardingSpec{Required: true, Engine: "vllm", DefaultShards: 2},
	}
}

// expertFixture is sglangFixture with the entry a manager writes for a vLLM gang.
func expertFixture(t *testing.T, workers ...*startedProcs) (*Orchestrator, []string) {
	t.Helper()
	o, _, nodes := sglangFixture(t, workers...)
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return o, ids
}

// PLAN T18.5: the two-part shape that ran on the design-partner cell — one
// card per machine — reaches each machine as exactly that line: rank 0 serves
// and owns the rendezvous, rank 1 is headless and starts at global rank 1.
func TestExpertGangStartsTheProvenRanks(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, ids := expertFixture(t, w0, w1)
	entry := vllmEntry()
	ctx := context.Background()

	if err := o.CreateSharded(ctx, entry, "g0", 0, ids, Parallelism{Expert: true, Flags: map[string]string{"max_model_len": "2048", "extra": "--enforce-eager"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	r0, r1 := w0.commands(), w1.commands()
	if len(r0) != 1 || len(r1) != 1 {
		t.Fatalf("one process per part: %v / %v", r0, r1)
	}
	rank0Host := hostOf(w0.srv.Listener.Addr().String())
	common := []string{
		"exec vllm serve 'vendor/Big-Model'",
		"--served-model-name 'big-model' 'vendor/Big-Model'",
		"--data-parallel-size 2 ",
		"--data-parallel-size-local 1 ",
		"--data-parallel-address " + rank0Host + " ",
		"--data-parallel-rpc-port 13345",
		"--enable-expert-parallel",
		"--gpu-memory-utilization \"$U\"",
		"'--max-model-len' '2048'",
		"'--enforce-eager'",
	}
	for _, c := range []string{r0[0], r1[0]} {
		if !strings.HasPrefix(c, "-lc "+gangIfnameShell) {
			t.Errorf("Gloo follows the worker's NCCL interface pin: %s", c)
		}
		for _, want := range common {
			if !strings.Contains(c, want) {
				t.Errorf("every rank carries %q: %s", want, c)
			}
		}
		if strings.Contains(c, "--tensor-parallel-size") || strings.Contains(c, "ray") {
			t.Errorf("one card per rank writes no tensor width and starts no Ray: %s", c)
		}
	}
	if !strings.Contains(r0[0], "--host "+rank0Host+" --port 9100") || strings.Contains(r0[0], "--headless") {
		t.Errorf("rank 0 serves the API on its own address: %s", r0[0])
	}
	if !strings.Contains(r1[0], "--headless --data-parallel-start-rank 1") || strings.Contains(r1[0], "--port ") {
		t.Errorf("rank 1 is headless and starts at global rank 1: %s", r1[0])
	}
	// Each rank's own address, so vLLM binds the interface the others dial.
	w0.mu.Lock()
	ip0 := w0.specs[0].Env["VLLM_HOST_IP"]
	w0.mu.Unlock()
	w1.mu.Lock()
	ip1 := w1.specs[0].Env["VLLM_HOST_IP"]
	w1.mu.Unlock()
	if ip0 != rank0Host || ip1 != hostOf(w1.srv.Listener.Addr().String()) {
		t.Errorf("VLLM_HOST_IP is each rank's own address: %q %q", ip0, ip1)
	}

	shards, err := o.Store.Shards().GetByGang(ctx, entry.ID, "g0")
	if err != nil {
		t.Fatal(err)
	}
	var coord, ranks int
	for _, s := range shards {
		switch s.Role {
		case "coordinator":
			coord++
			if s.Address != rank0Host+":9100" || !strings.Contains(s.ConfigJSON, `"vllm"`) {
				t.Errorf("the router dials rank 0's API with the vLLM driver: %+v", s)
			}
		case "rank":
			ranks++
		}
	}
	if coord != 1 || ranks != 1 {
		t.Errorf("a two-part expert gang records one coordinator and one rank, got %d and %d", coord, ranks)
	}
}

// k devices per part: every device is a data-parallel rank, and part r starts
// at global rank r × k. A tensor width inside the part divides both.
func TestExpertSplitArithmetic(t *testing.T) {
	for _, c := range []struct {
		par              Parallelism
		parts            int
		tp, dp, local    int
		wantErr, errText string
	}{
		{par: Parallelism{Expert: true}, parts: 2, tp: 1, dp: 2, local: 1},
		{par: Parallelism{Expert: true, DevicesPerRank: 4}, parts: 2, tp: 1, dp: 8, local: 4},
		{par: Parallelism{Expert: true, DevicesPerRank: 4, TP: 2}, parts: 2, tp: 2, dp: 4, local: 2},
		{par: Parallelism{Expert: true, DevicesPerRank: 8}, parts: 1, tp: 1, dp: 8, local: 8},
		{par: Parallelism{Expert: true, PP: 2}, parts: 2, errText: "no pipeline"},
		{par: Parallelism{Expert: true, DevicesPerRank: 2, TP: 4}, parts: 2, errText: "must divide"},
	} {
		s, err := c.par.expertSplit(c.parts)
		if c.errText != "" {
			if err == nil || !strings.Contains(err.Error(), c.errText) {
				t.Errorf("%+v over %d parts: want an error with %q, got %v", c.par, c.parts, c.errText, err)
			}
			continue
		}
		if err != nil || s.TP != c.tp || s.DP != c.dp || s.Local != c.local {
			t.Errorf("%+v over %d parts = %+v %v, want tp %d dp %d local %d", c.par, c.parts, s, err, c.tp, c.dp, c.local)
		}
	}

	r := vllmExpertCommand(vllmExpertRank{Model: "m", ServedAs: "m", Rank: 1, Parts: 2,
		Split: expertSplit{TP: 2, DP: 4, Local: 2}, Host: "10.0.0.3", Rank0Addr: "10.0.0.2", RPCPort: 13345})
	for _, want := range []string{"--tensor-parallel-size 2", "--data-parallel-size 4", "--data-parallel-size-local 2", "--data-parallel-start-rank 2", "--headless"} {
		if !strings.Contains(r, want) {
			t.Errorf("part 1 of two four-card parts at tp 2 carries %q: %s", want, r)
		}
	}
}

// One part holding every device is the in-machine shape: one process, nothing
// to dial — no address, no rpc port, no headless rank.
func TestExpertGangInsideOneMachine(t *testing.T) {
	c := vllmExpertCommand(vllmExpertRank{Model: "m", ServedAs: "m", Rank: 0, Parts: 1,
		Split: expertSplit{TP: 1, DP: 4, Local: 4}, Host: "10.0.0.2", ServePort: 9100})
	if !strings.Contains(c, "--data-parallel-size 4 --enable-expert-parallel --host 10.0.0.2 --port 9100") {
		t.Errorf("the in-machine shape is one process over every device: %s", c)
	}
	for _, never := range []string{"--data-parallel-address", "--data-parallel-rpc-port", "--data-parallel-size-local", "--headless"} {
		if strings.Contains(c, never) {
			t.Errorf("one part dials nothing (%s): %s", never, c)
		}
	}
}

// A shape this leader cannot launch is refused before the gang it would have
// replaced is touched: no process starts, and the answer is a 409-class
// refusal rather than an upstream failure that triggers the cleanup.
func TestExpertCreateRefusedBeforeAnythingStarts(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, ids := expertFixture(t, w0, w1)
	notVLLM := vllmEntry()
	notVLLM.Sharding.Engine = "sglang"
	for _, c := range []struct {
		name  string
		entry models.Entry
		par   Parallelism
	}{
		{"an engine without the backend", notVLLM, Parallelism{Expert: true}},
		{"a pipeline", vllmEntry(), Parallelism{Expert: true, PP: 2}},
	} {
		err := o.CreateSharded(context.Background(), c.entry, "g0", 0, ids, c.par)
		if !errors.Is(err, ErrUnplaceable) {
			t.Errorf("%s: want ErrUnplaceable, got %v", c.name, err)
		}
	}
	if n := len(w0.commands()) + len(w1.commands()); n != 0 {
		t.Errorf("a refused expert create starts nothing, started %d", n)
	}
}

// The head the create names is rank 0, whatever order the nodes came in.
func TestExpertGangHeadIsRankZero(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, ids := expertFixture(t, w0, w1)
	if err := o.CreateSharded(context.Background(), vllmEntry(), "g0", 0, ids, Parallelism{Expert: true, Head: ids[1]}); err != nil {
		t.Fatal(err)
	}
	if c := w1.commands(); len(c) != 1 || strings.Contains(c[0], "--headless") {
		t.Errorf("the named head serves: %v", c)
	}
	if c := w0.commands(); len(c) != 1 || !strings.Contains(c[0], "--headless --data-parallel-start-rank 1") {
		t.Errorf("the other part is rank 1: %v", c)
	}
}
