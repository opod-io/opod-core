package leader

import (
	"context"
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
	"github.com/opod-io/opod-core/pkg/nodeapi"
)

// T28.11: the block index tokenizes as deep as the prefix chain matches — 4
// bytes a token, never fewer than the 2,048 it always asked for.
func TestResolveBlocksTokenizesToThePrefixDepth(t *testing.T) {
	ctx := context.Background()
	tokens := make([]int, 32)
	srv, _, asked := prefixTestServer(t, tokens)
	srv.prefix.apply("w1", &nodeapi.KVBlocks{Seq: 1, BlockSize: 16, Stored: []string{"x"}})
	for i, c := range []struct{ depth, want int }{{0, 65536}, {4, 2048}, {1, 2048}, {64, 16384}, {4096, 262144}} {
		srv.router.SetPrefixDepth(c.depth)
		_ = srv.resolveBlocks(ctx, engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("p", i+1)}}})
		if got := asked.Load(); got != int64(c.want) {
			t.Errorf("depth %d KiB asked for %d tokens, want %d", c.depth, got, c.want)
		}
	}
}

// The chain cache key covers what the chain depends on: two prompts that
// differ past 64 KiB — inside the default depth — are two keys (before T28.11
// the key read 64 KiB and they shared a chain); two salts or two depths are
// two keys; the text is never the key.
func TestPrefixCacheKeyCoversTheDepth(t *testing.T) {
	head := strings.Repeat("h", 100<<10)
	a := engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: head + "a"}}}
	b := engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: head + "b"}}}
	ka := prefixCacheKey(a, "", 16, 65536)
	if ka == "" || ka == prefixCacheKey(b, "", 16, 65536) {
		t.Fatal("two prompts that differ at 100 KiB share a chain at 64 k tokens")
	}
	if ka != prefixCacheKey(a, "", 16, 65536) {
		t.Fatal("one prompt, one key")
	}
	if ka == prefixCacheKey(a, nodeapi.BlockSeed([]any{"S"}), 16, 65536) || ka == prefixCacheKey(a, "", 16, 2048) || ka == prefixCacheKey(a, "", 32, 65536) {
		t.Fatal("the seed, the token limit and the block size are part of the key")
	}
	// 2,048 tokens reach 64 KiB at most: the two share a chain there, as before.
	if prefixCacheKey(a, "", 16, 2048) != prefixCacheKey(b, "", 16, 2048) {
		t.Fatal("past what the tokens can reach the text does not change the key")
	}
	if prefixCacheKey(engines.ChatRequest{Model: "m"}, "", 16, 2048) != "" {
		t.Fatal("no prompt, no key")
	}
	if strings.Contains(ka, "hhhh") {
		t.Fatal("a key is a hash, never the text")
	}
	// Field boundaries are part of the key: a role/content split is not text.
	x := engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "user", Content: "ab"}}}
	y := engines.ChatRequest{Model: "m", Messages: []engines.Message{{Role: "usera", Content: "b"}}}
	if prefixCacheKey(x, "", 16, 2048) == prefixCacheKey(y, "", 16, 2048) {
		t.Fatal("two framings of one string share a key")
	}
}

// The chain cache stays inside both bounds — entries and the hashes they
// hold — with chains as deep as the default depth makes them, and its count
// of hashes is the sum of what it holds.
func TestPrefixChainCacheBounded(t *testing.T) {
	p := newPrefixIndex()
	deep := make([]string, 4096) // 65,536 tokens at block size 16
	now := time.Now()
	for i := 0; i < 4*prefixChainCacheHashes/len(deep); i++ {
		var k [8]byte
		binary.BigEndian.PutUint64(k[:], uint64(i))
		p.remember(string(k[:]), deep, now)
		if p.chainHashes > prefixChainCacheHashes || len(p.chains) > prefixChainCacheSize {
			t.Fatalf("insert %d: %d chains holding %d hashes, over the bounds", i, len(p.chains), p.chainHashes)
		}
	}
	sum := 0
	for _, e := range p.chains {
		sum += len(e.hashes)
	}
	if sum != p.chainHashes || len(p.chains) == 0 {
		t.Fatalf("held %d hashes in %d chains, counted %d", sum, len(p.chains), p.chainHashes)
	}
	// Replacing a key counts once.
	p.remember("again", deep[:10], now)
	p.remember("again", deep[:20], now)
	sum = 0
	for _, e := range p.chains {
		sum += len(e.hashes)
	}
	if sum != p.chainHashes {
		t.Fatalf("a replaced key: held %d, counted %d", sum, p.chainHashes)
	}
}
