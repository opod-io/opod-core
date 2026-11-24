package router

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/opod-io/opod-core/internal/engines"
)

// legacyPrefixKeysOf is prefixKeysOf as it was before T28.11 (core 37c9570),
// kept verbatim as the fixture depth 4 must reproduce byte for byte.
func legacyPrefixKeysOf(req engines.ChatRequest) []string {
	const capChars = 256 * 16
	var head []byte
	add := func(role, content string) {
		if len(head) >= capChars {
			return
		}
		head = append(head, role...)
		head = append(head, 0)
		// Clip BEFORE appending: nothing past the cap is ever hashed, so a
		// 1 MB prompt costs its head only.
		if room := capChars - len(head); len(content) > room {
			content = content[:max(room, 0)]
		}
		head = append(head, content...)
		head = append(head, 0)
	}
	if req.System != "" {
		add("system", req.System)
	}
	for _, m := range req.Messages {
		if m.Content != "" {
			add(m.Role, m.Content)
		}
	}
	if len(head) > capChars {
		head = head[:capChars]
	}
	if len(head) == 0 {
		return nil
	}
	keys := make([]string, 0, (len(head)+256-1)/256)
	var prev [sha256.Size]byte
	if req.CacheSalt != "" {
		prev = sha256.Sum256([]byte("opod-cache-salt\x00" + req.CacheSalt))
	}
	for off := 0; off < len(head); off += 256 {
		h := sha256.New()
		h.Write(prev[:])
		h.Write(head[off:min(off+256, len(head))])
		copy(prev[:], h.Sum(nil))
		keys = append(keys, hex.EncodeToString(prev[:8]))
	}
	return keys
}

// Depth 4 is the chain before T28.11, byte for byte, over the shapes its
// framing could drift on: no system prompt, a role past the cap, content
// clipped mid-chunk, empty turns, a salt, a prompt ending on a chunk edge.
func TestPrefixDepth4IsTheOldChain(t *testing.T) {
	reqs := []engines.ChatRequest{
		{},
		{System: "S"},
		{Messages: []engines.Message{{Role: "user", Content: "hi"}}},
		{System: strings.Repeat("s", 4090), Messages: []engines.Message{{Role: "assistant", Content: "after the cap"}}},
		{System: strings.Repeat("s", 4100), Messages: []engines.Message{{Role: "user", Content: "never read"}}},
		{System: strings.Repeat("s", 248)}, // "system\x00" + 248 + "\x00" = exactly one chunk
		{System: "sys", Messages: []engines.Message{{Role: "user", Content: ""}, {Role: "user", Content: strings.Repeat("u", 3000)}, {Role: "assistant", Content: strings.Repeat("a", 3000)}}},
		{System: strings.Repeat("Shared system prompt. ", 30), Messages: []engines.Message{{Role: "user", Content: "q"}}, CacheSalt: "salt-a"},
		{Messages: []engines.Message{{Role: strings.Repeat("r", 5000), Content: "x"}}},
	}
	for i, req := range reqs {
		if got, want := prefixKeysOf(req, 4), legacyPrefixKeysOf(req); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("request %d: depth 4 %v, the old chain %v", i, got, want)
		}
	}
}

// The deep chain (T28.11): dense to 4 KiB as before, then one link at each
// doubling distance to the cap. Two prompts that share 40 KiB match deeper
// than two that share 10 KiB; nothing past the cap is keyed.
func TestPrefixChainGoesDeepAndSparse(t *testing.T) {
	text := func(shared int, tail string) engines.ChatRequest {
		return engines.ChatRequest{Messages: []engines.Message{{Role: "user", Content: strings.Repeat("h", shared) + strings.Repeat(tail, 300<<10)}}}
	}
	common := func(a, b []string) int {
		n := 0
		for n < len(a) && n < len(b) && a[n] == b[n] {
			n++
		}
		return n
	}
	k40a, k40b := prefixKeysOf(text(40<<10, "a"), 0), prefixKeysOf(text(40<<10, "b"), 0)
	k10a, k10b := prefixKeysOf(text(10<<10, "a"), 0), prefixKeysOf(text(10<<10, "b"), 0)
	// 16 dense links, then 8, 16, 32, 64, 128 and 256 KiB.
	if len(k40a) != prefixMaxChunks+6 {
		t.Fatalf("a prompt past the cap has %d links, want %d", len(k40a), prefixMaxChunks+6)
	}
	if c40, c10 := common(k40a, k40b), common(k10a, k10b); c40 != prefixMaxChunks+3 || c10 != prefixMaxChunks+1 {
		t.Fatalf("shared 40 KiB match %d links (want to 32 KiB, %d), shared 10 KiB %d (want to 8 KiB, %d)", c40, prefixMaxChunks+3, c10, prefixMaxChunks+1)
	}
	// The dense head is the depth-4 chain; the tail only adds.
	if strings.Join(k40a[:prefixMaxChunks], ",") != strings.Join(prefixKeysOf(text(40<<10, "a"), 4), ",") {
		t.Fatal("the deep chain's head is not the depth-4 chain")
	}
	// A prompt that ends between links keys to its last full link.
	if got := prefixKeysOf(engines.ChatRequest{System: strings.Repeat("s", 20<<10)}, 0); len(got) != prefixMaxChunks+2 {
		t.Fatalf("a 20 KiB prompt has %d links, want to 16 KiB (%d)", len(got), prefixMaxChunks+2)
	}
	// A depth that is not a doubling keys its cap too, and reads no further.
	d := prefixKeysOf(text(40<<10, "a"), 20)
	if len(d) != prefixMaxChunks+3 || d[prefixMaxChunks+1] != k40a[prefixMaxChunks+1] {
		t.Fatalf("depth 20: %d links, want 8, 16 and 20 KiB past the head", len(d))
	}
	if e := prefixKeysOf(text(20<<10, "a"), 20); e[len(e)-1] != d[len(d)-1] {
		t.Fatal("depth 20 reads past 20 KiB")
	}
	// One salt, one chain, deep links included; two salts share none.
	sa, sb := text(40<<10, "a"), text(40<<10, "a")
	sa.CacheSalt, sb.CacheSalt = "salt-a", "salt-b"
	ka, kb := prefixKeysOf(sa, 0), prefixKeysOf(sb, 0)
	for i := range ka {
		if ka[i] == kb[i] || ka[i] == k40a[i] {
			t.Fatalf("link %d is shared across salts", i)
		}
	}
	// A short prompt is one partial link, as before.
	if got := prefixKeysOf(engines.ChatRequest{System: "S"}, 0); len(got) != 1 {
		t.Fatalf("a short prompt: %d links", len(got))
	}
}

// The policy's depth: 0 is the default, a value past the bound is the bound,
// and a router starts at the default.
func TestPrefixDepthPolicy(t *testing.T) {
	for in, want := range map[int]int{-1: DefaultPrefixDepthKiB, 0: DefaultPrefixDepthKiB, 4: 4, 64: 64, MaxPrefixDepthKiB + 1: MaxPrefixDepthKiB} {
		if got := PrefixDepthKiB(in); got != want {
			t.Fatalf("PrefixDepthKiB(%d) = %d, want %d", in, got, want)
		}
	}
	r := newLoadRouter(nil)
	if r.PrefixDepth() != DefaultPrefixDepthKiB {
		t.Fatalf("a router with no policy: depth %d", r.PrefixDepth())
	}
	r.SetPrefixDepth(4)
	if r.PrefixDepth() != 4 {
		t.Fatalf("set 4: %d", r.PrefixDepth())
	}
	r.SetPrefixDepth(0)
	if r.PrefixDepth() != DefaultPrefixDepthKiB {
		t.Fatalf("set 0: %d", r.PrefixDepth())
	}
}

// The pin table stays inside its bound at the deepest default chain: every
// served prompt pins 22 links.
func TestPrefixPinsBoundedAtDepth(t *testing.T) {
	r := newLoadRouter(nil)
	r.SetLoadAware(0, 0, true)
	n := prefixPinsMax/(prefixMaxChunks+6) + 64
	tail := strings.Repeat(".", 256<<10)
	for i := 0; i < n; i++ {
		ctx := withPrefixKeys(t.Context(), prefixKeysOf(engines.ChatRequest{System: fmt.Sprint("prompt ", i, tail)}, 0))
		r.rememberPrefix(ctx, "m", "w1")
		if len(r.prefixPins) > prefixPinsMax {
			t.Fatalf("insert %d left %d pins, over the bound %d", i, len(r.prefixPins), prefixPinsMax)
		}
	}
}
