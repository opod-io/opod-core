package router

// R9.4: load-aware and prefix-affine pick order, against a fake load table.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/store"
)

func rankedOrder(r *Router, ids ...string) []string {
	workers := make([]store.Placement, 0, len(ids))
	for _, id := range ids {
		workers = append(workers, store.Placement{NodeID: id})
	}
	r.mu.RLock()
	type ranked struct {
		sat bool
		sc  float64
	}
	rank := map[string]ranked{}
	for _, w := range workers {
		sat, sc := r.loadRank(w.NodeID)
		rank[w.NodeID] = ranked{sat, sc}
	}
	r.mu.RUnlock()
	// the same comparator pick() uses
	for i := 1; i < len(workers); i++ {
		for j := i; j > 0; j-- {
			a, b := rank[workers[j-1].NodeID], rank[workers[j].NodeID]
			less := false
			if a.sat != b.sat {
				less = !b.sat
			} else {
				less = b.sc < a.sc
			}
			if less {
				workers[j-1], workers[j] = workers[j], workers[j-1]
			}
		}
	}
	out := make([]string, 0, len(workers))
	for _, w := range workers {
		out = append(out, w.NodeID)
	}
	return out
}

func newLoadRouter(table map[string]LoadSignal) *Router {
	r := &Router{inflight: map[string]int{}, stickiness: map[string]stickyEntry{}}
	r.SetLoadSource(func(id string) (LoadSignal, bool) { s, ok := table[id]; return s, ok })
	return r
}

// With the weights off the order is the old one: in-flight only, even when
// the samples say a worker is full.
func TestLoadRank_OffIsInflightOnly(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{"full": {KVUsedPct: 99}, "idle": {KVUsedPct: 5}})
	r.inflight["full"], r.inflight["idle"] = 1, 2
	if got := rankedOrder(r, "idle", "full"); got[0] != "full" {
		t.Fatalf("weights off → in-flight order: %v", got)
	}
}

// The queue and the KV weight join the score.
func TestLoadRank_KVWeightAndQueue(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{"a": {KVUsedPct: 80, QueueDepth: 0}, "b": {KVUsedPct: 10, QueueDepth: 0}, "c": {KVUsedPct: 10, QueueDepth: 3}})
	r.SetLoadAware(4, 0, false) // a full cache = 4 requests
	r.inflight["a"], r.inflight["b"], r.inflight["c"] = 1, 2, 1
	// a: 1 + 4×0.8 = 4.2 · b: 2 + 0.4 = 2.4 · c: 1 + 3 + 0.4 = 4.4
	if got := rankedOrder(r, "a", "b", "c"); got[0] != "b" || got[1] != "a" || got[2] != "c" {
		t.Fatalf("score order b < a < c: %v", got)
	}
}

// A saturated worker stops receiving new requests before it errors: it goes
// behind every worker with headroom, whatever its in-flight count.
func TestLoadRank_SaturatedGoesLast(t *testing.T) {
	r := newLoadRouter(map[string]LoadSignal{"full": {KVUsedPct: 97}, "busy": {KVUsedPct: 60}})
	r.SetLoadAware(0, 95, false)
	r.inflight["full"], r.inflight["busy"] = 0, 5
	if got := rankedOrder(r, "full", "busy"); got[0] != "busy" {
		t.Fatalf("saturated last: %v", got)
	}
	// Every worker saturated → the least loaded of them.
	r2 := newLoadRouter(map[string]LoadSignal{"x": {KVUsedPct: 99}, "y": {KVUsedPct: 96}})
	r2.SetLoadAware(0, 95, false)
	r2.inflight["x"], r2.inflight["y"] = 3, 1
	if got := rankedOrder(r2, "x", "y"); got[0] != "y" {
		t.Fatalf("all saturated → least loaded: %v", got)
	}
	// No sample = no saturation, scored on in-flight alone.
	r3 := newLoadRouter(map[string]LoadSignal{})
	r3.SetLoadAware(4, 95, false)
	r3.inflight["p"], r3.inflight["q"] = 2, 1
	if got := rankedOrder(r3, "p", "q"); got[0] != "q" {
		t.Fatalf("unsampled → in-flight: %v", got)
	}
}

// The prefix chain hashes the leading text in chunks, each link covering
// everything before it: prompts share links for exactly as long as their
// text does, never the prompt itself, and a long document does not decide it.
func TestPrefixKeys(t *testing.T) {
	sys := strings.Repeat("You are a careful assistant. ", 40) // > 1 KiB: several links
	a := engines.ChatRequest{Messages: []engines.Message{{Role: "system", Content: sys}, {Role: "user", Content: "hello"}}}
	b := engines.ChatRequest{Messages: []engines.Message{{Role: "system", Content: sys}, {Role: "user", Content: "goodbye"}}}
	ka, kb := prefixKeysOf(a), prefixKeysOf(b)
	if len(ka) < 4 || len(ka) != len(kb) {
		t.Fatalf("a > 1 KiB lead is several links: %d %d", len(ka), len(kb))
	}
	shared := 0
	for shared < len(ka) && ka[shared] == kb[shared] {
		shared++
	}
	if shared != len(ka)-1 {
		t.Fatalf("the same system prompt shares every link but the last: %d of %d", shared, len(ka))
	}
	if strings.Contains(strings.Join(ka, ""), "assistant") {
		t.Fatal("a key is a hash, never the text")
	}
	if prefixKeysOf(engines.ChatRequest{}) != nil {
		t.Fatal("nothing to key on → no chain")
	}
	if len(prefixKeysOf(engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: "hi"}}})) != 1 {
		t.Fatal("a short prompt is one partial link")
	}
	long := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("y", 5000)}}}
	long2 := engines.ChatRequest{System: "S", Messages: []engines.Message{{Role: "user", Content: strings.Repeat("y", 6000)}}}
	if k := prefixKeysOf(long); len(k) != prefixMaxChunks || strings.Join(k, "") != strings.Join(prefixKeysOf(long2), "") {
		t.Fatal("the chain is the capped head, not the whole document")
	}
}

// Affinity: off by default; on, a served prefix pins every link to its worker
// for the TTL, per model; a request that shares only the lead matches on it.
func TestPrefixAffinity(t *testing.T) {
	r := newLoadRouter(nil)
	sys := strings.Repeat("Shared system prompt. ", 30)
	served := withPrefixKeys(context.Background(), prefixKeysOf(engines.ChatRequest{System: sys, Messages: []engines.Message{{Role: "user", Content: "first question"}}}))
	other := withPrefixKeys(context.Background(), prefixKeysOf(engines.ChatRequest{System: sys, Messages: []engines.Message{{Role: "user", Content: "a different question"}}}))
	r.rememberPrefix(served, "m", "w1")
	if got := r.prefixPick(served, "m", nil); got != "" {
		t.Fatalf("affinity off → no pin: %q", got)
	}
	r.SetLoadAware(0, 0, true)
	r.rememberPrefix(served, "m", "w1")
	if got := r.prefixPick(served, "m", nil); got != "w1" {
		t.Fatalf("pinned: %q", got)
	}
	if got := r.prefixPick(other, "m", nil); got != "w1" {
		t.Fatalf("the same system prompt with another question matches its lead: %q", got)
	}
	if got := r.prefixPick(served, "other", nil); got != "" {
		t.Fatalf("per model: %q", got)
	}
	if got := r.prefixPick(context.Background(), "m", nil); got != "" {
		t.Fatalf("no key → no pin: %q", got)
	}
	// other's own last link → w2; the lead it shares with served → w1 again
	// (each serve rewrites every link it covers).
	r.rememberPrefix(other, "m", "w2")
	r.rememberPrefix(served, "m", "w1")
	if got := r.prefixPick(other, "m", nil); got != "w2" {
		t.Fatalf("the longest link wins: %q", got)
	}
	if got := r.prefixPick(other, "m", func(id string) bool { return id != "w2" }); got != "w1" {
		t.Fatalf("an unusable longest pin falls back to a shorter link's worker: %q", got)
	}
	r.SetStickyTTL(time.Millisecond)
	r.rememberPrefix(served, "m", "w1")
	r.rememberPrefix(other, "m", "w1")
	time.Sleep(5 * time.Millisecond)
	if got := r.prefixPick(served, "m", nil); got != "" {
		t.Fatalf("expired: %q", got)
	}
}

// The pin table is bounded: an insert that takes it past prefixPinsMax cuts
// it to three quarters (expired pins first, then arbitrary ones), so it is
// never seen above the bound.
func TestPrefixPinsBounded(t *testing.T) {
	r := newLoadRouter(nil)
	r.SetLoadAware(0, 0, true)
	n := prefixPinsMax/prefixMaxChunks + 64 // more distinct leads than the table holds
	for i := 0; i < n; i++ {
		ctx := withPrefixKeys(context.Background(), prefixKeysOf(engines.ChatRequest{System: fmt.Sprint("prompt ", i, strings.Repeat(".", 4096))}))
		r.rememberPrefix(ctx, "m", "w1")
		if len(r.prefixPins) > prefixPinsMax {
			t.Fatalf("insert %d left %d pins, over the bound %d", i, len(r.prefixPins), prefixPinsMax)
		}
	}
	if got := len(r.prefixPins); got >= n*prefixMaxChunks || got < prefixPinsMax*3/4 {
		t.Fatalf("pins after %d leads: %d — want evicted, and no lower than three quarters", n, got)
	}
}

// pick() prefers the pinned worker only while it has headroom.
func TestPrefixPinYieldsToSaturation(t *testing.T) {
	table := map[string]LoadSignal{"w1": {KVUsedPct: 99}, "w2": {KVUsedPct: 10}}
	r := newLoadRouter(table)
	r.SetLoadAware(0, 95, true)
	ctx := withPrefixKeys(context.Background(), []string{"k"})
	r.rememberPrefix(ctx, "m", "w1")
	r.mu.RLock()
	sat, _ := r.loadRank("w1")
	r.mu.RUnlock()
	if !sat {
		t.Fatal("w1 is saturated")
	}
	workers := []store.Placement{{NodeID: "w2"}, {NodeID: "w1"}}
	if pin := r.prefixPick(ctx, "m", nil); pin != "" && !sat {
		workers = preferNode(workers, pin)
	}
	if workers[0].NodeID != "w2" {
		t.Fatalf("a saturated pin is not preferred: %v", workers)
	}
	table["w1"] = LoadSignal{KVUsedPct: 20}
	r.mu.RLock()
	sat, _ = r.loadRank("w1")
	r.mu.RUnlock()
	if pin := r.prefixPick(ctx, "m", nil); pin != "" && !sat {
		workers = preferNode(workers, pin)
	}
	if workers[0].NodeID != "w1" {
		t.Fatalf("with headroom the pin wins: %v", workers)
	}
}

// The cache salt seeds the prefix chain (T26.8): the same text under two salts
// shares no link, so one team's pin never draws another team's request to a
// worker whose engine holds that prefix only under the first team's salt;
// one salt keys alike; and no salt is the chain as it was before salts.
func TestPrefixKeysCarryTheSalt(t *testing.T) {
	sys := strings.Repeat("Shared system prompt. ", 30)
	req := func(salt string) engines.ChatRequest {
		return engines.ChatRequest{System: sys, Messages: []engines.Message{{Role: "user", Content: "q"}}, CacheSalt: salt}
	}
	plain, a, a2, b := prefixKeysOf(req("")), prefixKeysOf(req("salt-a")), prefixKeysOf(req("salt-a")), prefixKeysOf(req("salt-b"))
	if strings.Join(a, ",") != strings.Join(a2, ",") {
		t.Fatal("one salt, one chain")
	}
	for i := range a {
		if a[i] == b[i] || a[i] == plain[i] {
			t.Fatalf("link %d is shared across salts", i)
		}
	}
	// No salt: the first link is SHA-256 over a zero parent and the first
	// chunk, exactly as before the salt existed.
	head := []byte("system\x00" + sys)
	h := sha256.New()
	h.Write(make([]byte, sha256.Size))
	h.Write(head[:prefixChunkChars])
	if want := hex.EncodeToString(h.Sum(nil)[:8]); plain[0] != want {
		t.Fatalf("an unsalted chain changed: %s, want %s", plain[0], want)
	}

	r := newLoadRouter(nil)
	r.SetLoadAware(0, 0, true)
	r.rememberPrefix(withPrefixKeys(context.Background(), a), "m", "w1")
	if got := r.prefixPick(withPrefixKeys(context.Background(), a2), "m", nil); got != "w1" {
		t.Fatalf("the same salt follows its pin: %q", got)
	}
	if got := r.prefixPick(withPrefixKeys(context.Background(), b), "m", nil); got != "" {
		t.Fatalf("another salt must not follow it: %q", got)
	}
}
