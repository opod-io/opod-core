package router

import (
	"strings"
	"testing"

	"github.com/opod-io/opod-core/internal/engines"
)

// A 1 MB prompt keys on its first DefaultPrefixDepthKiB (the chain's cap,
// T19.23, deepened by T28.11) and must cost about that much to key: before
// PLAN T15.8 the whole message was copied to hash the head of it, and the
// chain now streams the text through its hashes without copying it at all.
func TestPrefixKeyOfALongPromptCostsItsHeadOnly(t *testing.T) {
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}}}
	short := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", DefaultPrefixDepthKiB<<10)}}}
	if strings.Join(prefixKeysOf(long, 0), ",") != strings.Join(prefixKeysOf(short, 0), ",") {
		t.Fatal("the chain is over the capped head, so a 1 MB prompt keys like its first 256 KiB")
	}
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = prefixKeysOf(long, 0)
		}
	})
	if got := res.AllocedBytesPerOp(); got > 16<<10 { // 22 keys and the slice that holds them
		t.Fatalf("keying a 1 MB prompt allocated %d B/op: the message is being copied", got)
	}
}

// BenchmarkPrefixKeyOfLongPrompt keys a 1 MB prompt at the default depth:
// the request path's cost, budgeted at 50 µs (PLAN T28.11).
func BenchmarkPrefixKeyOfLongPrompt(b *testing.B) {
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = prefixKeysOf(long, 0)
	}
}

// BenchmarkPrefixKeyOf256KiB keys a prompt exactly as deep as the default
// cap, as an agent's turn would be: several turns, a salt.
func BenchmarkPrefixKeyOf256KiB(b *testing.B) {
	turn := strings.Repeat("a tool call and its result. ", (32<<10)/28)
	req := engines.ChatRequest{System: strings.Repeat("You are an agent. ", 400), CacheSalt: "salt"}
	for len(req.Messages) < 8 {
		req.Messages = append(req.Messages, engines.Message{Role: "user", Content: turn})
	}
	b.ReportAllocs()
	b.SetBytes(256 << 10)
	for i := 0; i < b.N; i++ {
		_ = prefixKeysOf(req, 0)
	}
}

// BenchmarkPrefixKeyOfDepth4 is the chain as it was before T28.11.
func BenchmarkPrefixKeyOfDepth4(b *testing.B) {
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = prefixKeysOf(long, 4)
	}
}
