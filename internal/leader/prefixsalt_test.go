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

	"github.com/vmihailenco/msgpack/v5"

	"github.com/opod-io/opod-core/internal/config"
	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/internal/kvevents"
	"github.com/opod-io/opod-core/internal/store"
	"github.com/opod-io/opod-core/pkg/nodeapi"
)

// prefixTestServer is a leader with one reporting worker, w1, whose engine
// tokenizes every prompt to tokens and records the MaxTokens it was asked for.
func prefixTestServer(t *testing.T, tokens []int) (*Server, *atomic.Int32, *atomic.Int64) {
	t.Helper()
	cfg := config.Default()
	cfg.Listen = ":0"
	st, err := store.OpenSQLite(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := NewServer(cfg, st, &deadEngine{&stubLeaderEngine{}}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	var calls atomic.Int32
	var asked atomic.Int64
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != nodeapi.PathTokenize {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var in nodeapi.TokenizeRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		asked.Store(int64(in.MaxTokens))
		_ = json.NewEncoder(w).Encode(nodeapi.TokenizeResponse{Tokens: tokens})
	}))
	t.Cleanup(worker.Close)
	if err := st.Nodes().Upsert(context.Background(), store.Node{ID: "w1", Hostname: "w1", State: "ready", Address: strings.TrimPrefix(worker.URL, "http://"), WorkerToken: "wtok", LastHeartbeat: time.Now()}); err != nil {
		t.Fatal(err)
	}
	return srv, &calls, &asked
}

// vllmStored is the BlockStored vLLM 0.27.1 publishes for a prompt's first
// blocks, its block-0 keys in extra_keys (generate_block_hash_extra_keys).
func vllmStored(t *testing.T, tokens []int, block0 []any) []byte {
	t.Helper()
	hashes := make([]any, len(tokens)/16)
	extra := make([]any, len(hashes))
	for i := range hashes {
		hashes[i] = uint64(1000 + i)
	}
	if len(block0) > 0 {
		extra[0] = block0
	}
	ev := map[string]any{"type": "BlockStored", "block_hashes": hashes, "parent_block_hash": nil, "token_ids": tokens,
		"block_size": int8(16), "lora_name": nil, "medium": "GPU", "extra_keys": extra}
	b, err := msgpack.Marshal([]any{1790000000.5, []any{ev}, int8(0)})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// T28.9: two callers with one prompt and two salts. The worker re-hashes its
// engine's events (as kvevents does on the worker) and the leader resolves
// each caller's chain from the salt it sent: the worker that holds caller A's
// blocks earns no credit for B, earns it again for A, and a request with no
// salt resolves the chain it always did.
func TestResolveBlocksIsSaltedPerCaller(t *testing.T) {
	ctx := context.Background()
	tokens := make([]int, 40)
	for i := range tokens {
		tokens[i] = 100 + i
	}
	srv, calls, _ := prefixTestServer(t, tokens)

	// w1's engine stored caller A's prompt under A's salt.
	tr := kvevents.NewTranslator()
	if err := tr.Frame(0, vllmStored(t, tokens[:32], []any{"salt-a"})); err != nil {
		t.Fatal(err)
	}
	srv.prefix.apply("w1", tr.Drain())

	req := func(salt string) engines.ChatRequest {
		return engines.ChatRequest{Model: "m", System: "you are terse", Messages: []engines.Message{{Role: "user", Content: "hello"}}, CacheSalt: salt}
	}
	a, b := srv.resolveBlocks(ctx, req("salt-a")), srv.resolveBlocks(ctx, req("salt-b"))
	if n := srv.prefix.leading("w1", a); n != 2 {
		t.Fatalf("caller A's chain: w1 holds %d leading blocks, want 2", n)
	}
	if n := srv.prefix.leading("w1", b); n != 0 {
		t.Fatalf("caller B's chain: w1 is credited %d blocks it holds under A's salt", n)
	}
	if n := srv.prefix.leading("w1", srv.resolveBlocks(ctx, req("salt-a"))); n != 2 {
		t.Fatalf("A again: %d", n)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("the chain cache must key by the salt: %d tokenizes for two callers, want 2", n)
	}
	// No salt: exactly the chain before salts.
	if got, want := srv.resolveBlocks(ctx, req("")), nodeapi.BlockHashes("", tokens, 16); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("an unsalted chain moved: %v, want %v", got, want)
	}
}

// The seed's keys: the adapter of a "<model>:<adapter>" variant of the plan's
// model, then the salt, in vLLM's order; with no plan there is no base to split
// an adapter from.
func TestBlockSeedOf(t *testing.T) {
	s := &Server{}
	if got := s.blockSeedOf(engines.ChatRequest{Model: "qwen:support", CacheSalt: "S"}); got != nodeapi.BlockSeed([]any{"S"}) {
		t.Fatal("no plan: the salt alone")
	}
	s.plan.modelID = "qwen"
	for _, c := range []struct {
		model, salt string
		keys        []any
	}{
		{"qwen", "", nil},
		{"qwen", "S", []any{"S"}},
		{"qwen:support", "", []any{"support"}},
		{"qwen:support", "S", []any{"support", "S"}},
		{"qwen:", "S", []any{"S"}},
		{"other:support", "S", []any{"S"}},
	} {
		if got := s.blockSeedOf(engines.ChatRequest{Model: c.model, CacheSalt: c.salt}); got != nodeapi.BlockSeed(c.keys) {
			t.Errorf("%s / %q: seed of %v expected", c.model, c.salt, c.keys)
		}
	}
	// The worker's view of the same adapter request agrees (vLLM lists the
	// LoRA name before the salt).
	tr := kvevents.NewTranslator()
	toks := make([]int, 16)
	if err := tr.Frame(0, vllmStored(t, toks, []any{"support", "S"})); err != nil {
		t.Fatal(err)
	}
	if got := tr.Drain().Stored[0]; got != nodeapi.BlockHashes(s.blockSeedOf(engines.ChatRequest{Model: "qwen:support", CacheSalt: "S"}), toks, 16)[0] {
		t.Fatal("the worker and the leader disagree on an adapter's salted block")
	}
}
