package router

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/store"
)

// scriptEngine is a worker whose Chat does what the test says.
type scriptEngine struct {
	engines.Engine
	chat func(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error)
}

func (e *scriptEngine) Name() string { return "script" }
func (e *scriptEngine) Chat(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	return e.chat(ctx, req)
}

// gatedEngine holds every answer before its first event until the test
// opens the gate, and counts the requests waiting there.
type gatedEngine struct {
	scriptEngine
	gate    chan struct{}
	waiting chan struct{}
}

func newGatedEngine() *gatedEngine {
	g := &gatedEngine{gate: make(chan struct{}), waiting: make(chan struct{}, 64)}
	g.chat = func(ctx context.Context, _ engines.ChatRequest) (<-chan engines.StreamEvent, error) {
		out := make(chan engines.StreamEvent, 2)
		go func() {
			defer close(out)
			g.waiting <- struct{}{}
			select {
			case <-g.gate:
			case <-ctx.Done():
				return
			}
			out <- engines.StreamEvent{Delta: "ok"}
			out <- engines.StreamEvent{Done: true}
		}()
		return out, nil
	}
	return g
}

// promptOf is a chat request whose prompt is about tokens long under the
// bytes ÷ 4 estimate.
func promptOf(tokens int) engines.ChatRequest {
	return engines.ChatRequest{Model: "m", Stream: true,
		Messages: []engines.Message{{Role: "user", Content: strings.Repeat("abcd", tokens)}}}
}

func drainAll(stream <-chan engines.StreamEvent) {
	for range stream {
	}
}

// The row's L1 proof (PLAN T28.10): a 100 000-token request and a 50-token one
// in prefill on two idle workers send the next long request to the worker
// with the short one. With the weight off both count one request, the tie
// keeps placement order, and the long request lands behind the other long
// one — today's order.
func TestALongPrefillSendsTheNextLongRequestToTheOtherWorker(t *testing.T) {
	for _, tc := range []struct {
		weight float64
		want   string
	}{{0, "w0"}, {1, "w1"}} {
		t.Run(fmt.Sprintf("weight %v", tc.weight), func(t *testing.T) {
			r, _ := drainRouter(t,
				store.Node{ID: "w0", State: store.NodeStateReady},
				store.Node{ID: "w1", State: store.NodeStateReady})
			r.SetTokenWeight(tc.weight, nil)
			g0, g1 := newGatedEngine(), newGatedEngine()
			r.remotes["w0"], r.remotes["w1"] = g0, g1
			ctx := context.Background()

			var wg sync.WaitGroup
			send := func(req engines.ChatRequest, on *gatedEngine) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s, err := r.Chat(ctx, req)
					if err != nil {
						t.Error(err)
						return
					}
					drainAll(s)
				}()
				select {
				case <-on.waiting:
				case <-time.After(5 * time.Second):
					t.Fatal("the request did not reach the worker it was expected on")
				}
			}
			send(promptOf(100_000), g0) // both idle: placement order
			send(promptOf(50), g1)      // w0 has one in flight, w1 none

			if _, node, _ := r.pick(ctx, "m"); node != tc.want {
				t.Fatalf("the next long request went to %s, want %s (ledger %v)", node, tc.want, r.prefillTokens())
			}
			close(g0.gate)
			close(g1.gate)
			wg.Wait()
			if got := r.prefillTokens(); len(got) != 0 {
				t.Fatalf("tokens left on the ledger after every first event: %v", got)
			}
		})
	}
}

// Weight 0 ranks exactly as today: nothing is held, and whatever the ledger
// holds adds nothing to a score.
func TestTokenWeightZeroRanksAsToday(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 200; trial++ {
		r := New(nil, nil)
		sampled := time.Now()
		loads := map[string]LoadSignal{}
		cands := make([]candidate, 4)
		for i := range cands {
			id := fmt.Sprintf("w%d", i)
			cands[i] = workerCandidate(id, false)
			r.inflight[id] = rng.Intn(4)
			loads[id] = LoadSignal{QueueDepth: int64(rng.Intn(3)), KVUsedPct: float64(rng.Intn(100)), SampledAt: sampled}
		}
		r.SetLoadSource(func(id string) (LoadSignal, bool) { s, ok := loads[id]; return s, ok })
		r.SetLoadAware(float64(rng.Intn(3)), 90, false)
		before, _ := r.rankCandidates(context.Background(), "m", cands, nil)

		r.SetTokenWeight(0, nil)
		for _, c := range cands {
			if h := r.holdPrefill(c.key, 1+rng.Intn(100_000)); h != nil {
				t.Fatal("weight 0 took a hold")
			}
			r.prefill[c.key] = rng.Intn(100_000) // even a ledger left over weighs nothing
		}
		after, _ := r.rankCandidates(context.Background(), "m", cands, nil)
		for i := range before {
			if before[i].key != after[i].key {
				t.Fatalf("trial %d: weight 0 changed the order: %v → %v", trial, keysOf(before), keysOf(after))
			}
		}
	}
}

func keysOf(cs []candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.key
	}
	return out
}

// The ledger's invariant (property test over random orders): it holds
// exactly the tokens of the holds not yet released, a hold releases once
// however many paths release it, and after every hold is released it is
// empty — whatever order dispatch, first event, completion, failure and
// cancel came in.
func TestThePrefillLedgerReturnsToZeroInAnyOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	keys := []string{"w0", "w1", gangKey("m", "g0")}
	for trial := 0; trial < 500; trial++ {
		r := New(nil, nil)
		r.SetTokenWeight(1, nil)
		type live struct {
			key    string
			tokens int
			h      *prefillHold
		}
		var holds []live
		want := map[string]int{}
		check := func(step string) {
			got := r.prefillTokens()
			for k, v := range got {
				if v <= 0 || v != want[k] {
					t.Fatalf("trial %d after %s: ledger %v, want %v", trial, step, got, want)
				}
			}
			for k, v := range want {
				if v != 0 && got[k] != v {
					t.Fatalf("trial %d after %s: ledger %v, want %v", trial, step, got, want)
				}
			}
		}
		for step := 0; step < 60; step++ {
			switch op := rng.Intn(5); {
			case op <= 1 || len(holds) == 0: // dispatch
				k, n := keys[rng.Intn(len(keys))], 1+rng.Intn(50_000)
				holds = append(holds, live{k, n, r.holdPrefill(k, n)})
				want[k] += n
				check("dispatch")
			default: // first event, completion, failure or cancel: one or several releases
				i := rng.Intn(len(holds))
				h := holds[i]
				for n := 1 + rng.Intn(3); n > 0; n-- {
					h.h.Release()
				}
				want[h.key] -= h.tokens
				holds = append(holds[:i], holds[i+1:]...)
				check("release")
			}
		}
		for _, h := range rng.Perm(len(holds)) {
			holds[h].h.Release()
		}
		if got := r.prefillTokens(); len(got) != 0 {
			t.Fatalf("trial %d: ledger %v after every hold was released", trial, got)
		}
	}
}

// A node removed with holds out (InvalidateNode), or the weight turned off
// with holds out, never leaves a token behind or a negative count.
func TestTheLedgerSurvivesARemovedNodeAndAPolicyChange(t *testing.T) {
	r := New(nil, nil)
	r.SetTokenWeight(1, nil)
	a, b := r.holdPrefill("w0", 100), r.holdPrefill("w1", 50)
	r.InvalidateNode("w0")
	r.SetTokenWeight(0, nil)
	c := r.holdPrefill("w1", 10) // off: holds nothing
	a.Release()
	b.Release()
	c.Release()
	if got := r.prefillTokens(); len(got) != 0 {
		t.Fatalf("ledger %v, want empty", got)
	}
}

// Through the dispatch path itself: requests that fail before the engine
// answers, break before their first event, close with none, are cancelled
// by their caller before or after it, or finish — at random, concurrently,
// on three workers — leave the ledger empty once every Chat has returned.
func TestEveryDispatchPathReleasesItsTokens(t *testing.T) {
	r, _ := drainRouter(t,
		store.Node{ID: "w0", State: store.NodeStateReady},
		store.Node{ID: "w1", State: store.NodeStateReady},
		store.Node{ID: "w2", State: store.NodeStateReady})
	r.SetTokenWeight(1, nil)
	behaviours := []func(ctx context.Context) (<-chan engines.StreamEvent, error){
		func(context.Context) (<-chan engines.StreamEvent, error) { // fails before answering
			return nil, errors.New("dial: refused")
		},
		func(context.Context) (<-chan engines.StreamEvent, error) { // breaks before the first event
			out := make(chan engines.StreamEvent, 1)
			out <- engines.StreamEvent{Err: engines.ErrStreamBroken}
			close(out)
			return out, nil
		},
		func(context.Context) (<-chan engines.StreamEvent, error) { // closes with no event
			out := make(chan engines.StreamEvent)
			close(out)
			return out, nil
		},
		func(ctx context.Context) (<-chan engines.StreamEvent, error) { // waits for its caller to leave
			out := make(chan engines.StreamEvent)
			go func() { <-ctx.Done(); close(out) }()
			return out, nil
		},
		func(context.Context) (<-chan engines.StreamEvent, error) { // answers whole
			out := make(chan engines.StreamEvent, 3)
			out <- engines.StreamEvent{Delta: "a"}
			out <- engines.StreamEvent{Delta: "b"}
			out <- engines.StreamEvent{Done: true}
			close(out)
			return out, nil
		},
	}
	rng := rand.New(rand.NewSource(3))
	var mu sync.Mutex
	pick := func() int { mu.Lock(); defer mu.Unlock(); return rng.Intn(len(behaviours)) }
	for _, id := range []string{"w0", "w1", "w2"} {
		r.remotes[id] = &scriptEngine{chat: func(ctx context.Context, _ engines.ChatRequest) (<-chan engines.StreamEvent, error) {
			return behaviours[pick()](ctx)
		}}
	}
	// Every worker failing sends a request to the leader's own engine, which
	// must take concurrent calls too.
	r.local = &scriptEngine{chat: func(ctx context.Context, _ engines.ChatRequest) (<-chan engines.StreamEvent, error) {
		return behaviours[len(behaviours)-1](ctx)
	}}
	var wg sync.WaitGroup
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(1+i%5)*time.Millisecond)
			defer cancel()
			s, err := r.Chat(ctx, promptOf(1+i*37))
			if err == nil {
				if i%2 == 0 {
					cancel() // the caller leaves mid-answer
				}
				drainAll(s)
			}
		}(i)
	}
	wg.Wait()
	if got := r.prefillTokens(); len(got) != 0 {
		t.Fatalf("tokens left on the ledger after every request returned: %v", got)
	}
}

// The estimate: bytes ÷ 4, the block index's count when it is more, less the
// blocks the target holds — never below 0, never a block taken off with no
// block size.
func TestTheUncachedEstimate(t *testing.T) {
	r := New(nil, nil)
	req := promptOf(1000) // 4,000 bytes
	if got := r.uncachedTokens(context.Background(), req, "w0"); got != 1000 {
		t.Fatalf("no chain: %d, want bytes ÷ 4 = 1000", got)
	}
	ctx := context.WithValue(context.Background(), prefixBlocksCtx{}, make([]string, 80)) // 80 blocks
	held := map[string]int{"w0": 30, "w1": 0}
	r.SetPrefixBlocks(1, nil, func(id string, _ []string) int { return held[id] })
	if got := r.uncachedTokens(ctx, req, "w0"); got != 1000 {
		t.Fatalf("a chain with no block size: %d, want 1000", got)
	}
	r.SetTokenWeight(1, func(string) int { return 16 })
	if got := r.uncachedTokens(ctx, req, "w1"); got != 1280 {
		t.Fatalf("80 blocks × 16 = 1280 tokens is more than bytes ÷ 4: got %d", got)
	}
	if got := r.uncachedTokens(ctx, req, "w0"); got != 1280-30*16 {
		t.Fatalf("w0 holds 30 blocks: got %d, want %d", got, 1280-30*16)
	}
	held["w0"] = 500
	if got := r.uncachedTokens(ctx, req, "w0"); got != 0 {
		t.Fatalf("held more than the estimate: got %d, want 0", got)
	}
	kv := req
	kv.KVTransfer = []byte(`{"remote":1}`)
	if got := r.dispatchTokens(ctx, "w0", kv); got != 0 {
		t.Fatalf("a decode half handed its KV computes no prefill: got %d", got)
	}
	r.SetTokenWeight(0, nil)
	if got := r.dispatchTokens(ctx, "w0", req); got != -1 {
		t.Fatalf("with nothing reading it the estimate is skipped: got %d", got)
	}
}
