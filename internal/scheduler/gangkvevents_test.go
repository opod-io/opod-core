package scheduler

import (
	"context"
	"strings"
	"testing"

	"github.com/opod-io/opod-core/internal/agent"
	"github.com/opod-io/opod-core/internal/store"
)

// subscribes marks a node as a worker started with OPOD_KV_EVENTS=1, the way
// its registration does (agent.Capabilities.KVEvents).
func subscribes(t *testing.T, o *Orchestrator, n store.Node) {
	t.Helper()
	n.HardwareJSON = `{"Engine":"sglang","KVEvents":true}`
	if err := o.Store.Nodes().Upsert(context.Background(), n); err != nil {
		t.Fatal(err)
	}
}

// PLAN T26.22: a gang whose head subscribes to KV events is launched with the
// engine's publisher. It was launched with none, so a gang with kvEvents: true
// served with no events and the leader routed it by load alone. Every SGLang
// rank carries the same flags (the page size must agree across the group);
// only rank 0's worker is told to subscribe.
func TestAnSGLangGangHeadedByASubscriberPublishesItsKVEvents(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, entry, nodes := sglangFixture(t, w0, w1)
	subscribes(t, o, nodes[0])
	if err := o.CreateSharded(context.Background(), entry, "g0", 0, []string{nodes[0].ID, nodes[1].ID}, Parallelism{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	want := agent.KVEventsArgs("sglang", "")
	for rank, w := range []*startedProcs{w0, w1} {
		cmds := w.commands()
		if len(cmds) != 1 || !strings.Contains(cmds[0], want) {
			t.Errorf("rank %d lacks the publisher flags %q: %v", rank, want, cmds)
		}
		if strings.Count(cmds[0], "--page-size") != 1 {
			t.Errorf("rank %d: one page size, from the publisher rule: %s", rank, cmds[0])
		}
	}
	if !w0.specs[0].KVEvents || w1.specs[0].KVEvents {
		t.Errorf("only rank 0's worker subscribes: rank0=%v rank1=%v", w0.specs[0].KVEvents, w1.specs[0].KVEvents)
	}
}

// A head that does not subscribe gets no flag: the plan's page size and the
// engine's defaults stay as they were.
func TestAGangWhoseHeadDoesNotSubscribeIsLaunchedAsBefore(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, entry, nodes := sglangFixture(t, w0, w1)
	subscribes(t, o, nodes[1]) // rank 1 subscribing does not make the gang publish
	if err := o.CreateSharded(context.Background(), entry, "g0", 0, []string{nodes[0].ID, nodes[1].ID}, Parallelism{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	for _, c := range append(w0.commands(), w1.commands()...) {
		if strings.Contains(c, "--kv-events-config") || strings.Contains(c, "--page-size") {
			t.Errorf("a gang whose head does not subscribe got the publisher: %s", c)
		}
	}
	if w0.specs[0].KVEvents || w1.specs[0].KVEvents {
		t.Error("no worker is told to subscribe when the head does not")
	}
}

// The vLLM Ray head's serve line carries the publisher when its worker
// subscribes, by the one rule a worker's own launch uses — the
// self-describing transfer config included when the plan gives a CPU tier.
func TestAVLLMRayGangHeadPublishesItsKVEvents(t *testing.T) {
	extra := "'--kv-offloading-size' '8'"
	kv := agent.KVEventsArgs("vllm", extra)
	if !strings.Contains(kv, "--kv-events-config") || !strings.Contains(kv, "--kv-transfer-config") {
		t.Fatalf("the vLLM rule with a CPU tier: %q", kv)
	}
	got := vllmGangCommand(vllmGang{Model: "org/model", ServedAs: "m", Host: "10.0.0.1", Port: 9100, TP: 1, PP: 2, Extra: extra, KVEvents: kv})
	if !strings.Contains(got, kv) || strings.Index(got, kv) < strings.Index(got, "exec vllm serve") {
		t.Errorf("the serve line lacks the publisher flags:\n%s", got)
	}
	if off := vllmGangCommand(vllmGang{Model: "org/model", ServedAs: "m", Host: "10.0.0.1", Port: 9100, TP: 1, PP: 2}); strings.Contains(off, "--kv-events-config") {
		t.Errorf("no subscriber, no publisher:\n%s", off)
	}
	for _, tc := range []struct {
		hw   string
		want bool
	}{{`{"KVEvents":true}`, true}, {`{"Engine":"vllm"}`, false}, {"", false}, {"not json", false}} {
		if got := kvEventsOn(store.Node{HardwareJSON: tc.hw}); got != tc.want {
			t.Errorf("kvEventsOn(%q) = %v, want %v", tc.hw, got, tc.want)
		}
	}
}

// The Ray create wires it: the head's vllm serve carries the publisher and
// only that spec tells its worker to subscribe; the Ray daemons do not.
func TestAVLLMRayGangCreateHeadedByASubscriberPublishes(t *testing.T) {
	w0, w1 := newFakeWorker(t, 0), newFakeWorker(t, 0)
	o, ids := expertFixture(t, w0, w1)
	nodes, err := o.Store.Nodes().List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range nodes {
		if n.ID == ids[0] {
			subscribes(t, o, n)
		}
	}
	if err := o.CreateSharded(context.Background(), vllmEntry(), "g0", 0, ids, Parallelism{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	var serve *agent.ProcessSpec
	for i, s := range w0.specs {
		if strings.Contains(strings.Join(s.Args, " "), "vllm serve") {
			serve = &w0.specs[i]
		} else if s.KVEvents {
			t.Errorf("a Ray daemon was told to subscribe: %s", s.ID)
		}
	}
	if serve == nil {
		t.Fatalf("no vllm serve on the head: %v", w0.commands())
	}
	if !serve.KVEvents || !strings.Contains(strings.Join(serve.Args, " "), agent.KVEventsArgs("vllm", "")) {
		t.Errorf("the head's serve line lacks the publisher or the subscribe mark: %+v", serve)
	}
	for _, s := range w1.specs {
		if s.KVEvents || strings.Contains(strings.Join(s.Args, " "), "--kv-events-config") {
			t.Errorf("a Ray worker rank got the publisher: %s", s.ID)
		}
	}
}
