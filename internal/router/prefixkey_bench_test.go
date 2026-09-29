package router

import (
	"strings"
	"testing"

	"github.com/opod-io/opod/internal/engines"
)

// A 1 MB prompt keys on its first KiB and must cost about that much to key:
// before PLAN T15.8 the whole message was copied to hash 1024 bytes of it.
func TestPrefixKeyOfALongPromptCostsItsHeadOnly(t *testing.T) {
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}}}
	short := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", 1024)}}}
	if prefixKeyOf(long) != prefixKeyOf(short) {
		t.Fatal("the key is over the capped head, so a 1 MB prompt keys like its first KiB")
	}
	res := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			_ = prefixKeyOf(long)
		}
	})
	if got := res.AllocedBytesPerOp(); got > 8<<10 {
		t.Fatalf("keying a 1 MB prompt allocated %d B/op: the message is being copied whole", got)
	}
}

func BenchmarkPrefixKeyOfLongPrompt(b *testing.B) {
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("x", 1<<20)}}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = prefixKeyOf(long)
	}
}
