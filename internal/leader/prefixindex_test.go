package leader

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/opod-io/opod-sdk/nodeapi"

	"github.com/opod-io/opod/internal/config"
	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

// The index holds what each worker's engine said it holds, counts LEADING
// blocks only, and empties a worker's set rather than guess — on a gap in its
// sequence, on its own `cleared`, and when it is forgotten.
func TestPrefixIndexFollowsTheEngines(t *testing.T) {
	p := newPrefixIndex()
	if p.state() != nil {
		t.Fatal("nothing reported, nothing to say")
	}
	p.apply("w1", &nodeapi.KVBlocks{Seq: 1, BlockSize: 16, Stored: []string{"a", "b", "c"}})
	p.apply("w2", &nodeapi.KVBlocks{Seq: 1, BlockSize: 16, Stored: []string{"a"}})
	chain := []string{"a", "b", "c", "d"}
	if got := p.leading("w1", chain); got != 3 {
		t.Fatalf("w1 holds a,b,c: %d", got)
	}
	if got := p.leading("w2", chain); got != 1 {
		t.Fatalf("w2 holds a: %d", got)
	}
	// A block held past a missing one is not a hit: the cache is a chain.
	p.apply("w1", &nodeapi.KVBlocks{Seq: 2, Removed: []string{"b"}})
	if got := p.leading("w1", chain); got != 1 {
		t.Fatalf("b evicted: only a leads, got %d", got)
	}
	if st := p.state(); st == nil || st.WorkersReporting != 2 || st.Blocks != 3 {
		t.Fatalf("state %+v, want 2 workers and 3 blocks", st)
	}
	// A batch we never saw (seq 2 → 4): what we hold may be wrong, so it goes.
	p.apply("w1", &nodeapi.KVBlocks{Seq: 4, Stored: []string{"x"}})
	if p.leading("w1", chain) != 0 || p.leading("w1", []string{"x"}) != 1 {
		t.Fatal("a gap must empty the worker's set and keep only what the batch stored")
	}
	// The worker's own word.
	p.apply("w2", &nodeapi.KVBlocks{Seq: 2, Cleared: true, Stored: []string{"z"}})
	if p.leading("w2", chain) != 0 || p.leading("w2", []string{"z"}) != 1 {
		t.Fatal("cleared must forget, then apply what came with it")
	}
	p.forget("w2")
	if p.leading("w2", []string{"z"}) != 0 {
		t.Fatal("a forgotten worker holds nothing")
	}
}

// The resolver asks a REPORTING worker's engine to tokenize, hashes the
// leading tokens with the shared function, and asks once per distinct prefix.
func TestResolveBlocksTokenizesOncePerPrefix(t *testing.T) {
	ctx := context.Background()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)

	tokens := make([]int, 40)
	for i := range tokens {
		tokens[i] = 100 + i
	}
	var calls atomic.Int32
	var sawText atomic.Bool
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeapi.PathTokenize {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var in nodeapi.TokenizeRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		if len(in.Messages) == 2 && in.Messages[0].Role == "system" && in.Messages[1].Content == "hello" {
			sawText.Store(true)
		}
		_ = json.NewEncoder(w).Encode(nodeapi.TokenizeResponse{Tokens: tokens})
	}))
	defer worker.Close()
	if err := st.Nodes().Upsert(ctx, store.Node{ID: "w1", Hostname: "w1", State: "ready", Address: strings.TrimPrefix(worker.URL, "http://"), WorkerToken: "wtok", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	req := engines.ChatRequest{Model: "m", System: "you are terse", Messages: []engines.Message{{Role: "user", Content: "hello"}}}

	// Nobody reports blocks: there is nothing to score against, so no tokenize.
	if got := srv.resolveBlocks(ctx, req); got != nil || calls.Load() != 0 {
		t.Fatalf("no reporter: %v after %d calls", got, calls.Load())
	}
	want := nodeapi.BlockHashes(tokens, 16)
	srv.prefix.apply("w1", &nodeapi.KVBlocks{Seq: 1, BlockSize: 16, Stored: want[:1]})
	got := srv.resolveBlocks(ctx, req)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("chain %v, want %v", got, want)
	}
	if !sawText.Load() {
		t.Fatal("the worker was not sent the prompt to tokenize")
	}
	_ = srv.resolveBlocks(ctx, req)
	if n := calls.Load(); n != 1 {
		t.Fatalf("the same prefix was tokenized %d times, want once", n)
	}
	if srv.prefix.leading("w1", got) != 1 {
		t.Fatal("w1 holds the first block of this prompt")
	}
	// A different prompt is a different key.
	req.Messages[0].Content = "goodbye"
	_ = srv.resolveBlocks(ctx, req)
	if n := calls.Load(); n != 2 {
		t.Fatalf("a new prefix must be tokenized: %d calls", n)
	}
}
