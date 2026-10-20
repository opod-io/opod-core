package router

import (
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// A 1 MB prompt keys on its first 4 KiB (the chain's cap, T19.23) and must
// cost about that much to key: before PLAN T15.8 the whole message was copied
// to hash the head of it.
func TestPrefixKeyOfALongPromptCostsItsHeadOnly(t *testing.T) {
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}}}
	short := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", prefixChunkChars*prefixMaxChunks)}}}
	if strings.Join(prefixKeysOf(long), ",") != strings.Join(prefixKeysOf(short), ",") {
		t.Fatal("the chain is over the capped head, so a 1 MB prompt keys like its first 4 KiB")
	}
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = prefixKeysOf(long)
		}
	})
	if got := res.AllocedBytesPerOp(); got > 16<<10 { // the 4 KiB head, 16 hashes and their keys
		t.Fatalf("keying a 1 MB prompt allocated %d B/op: the message is being copied whole", got)
	}
}

func BenchmarkPrefixKeyOfLongPrompt(b *testing.B) {
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = prefixKeysOf(long)
	}
}
