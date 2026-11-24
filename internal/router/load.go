package router

// Load-aware and prefix-affine worker choice (ADR-040 gap G3;
// feature "routing_load_aware"). The picker used to order workers by the
// leader's own in-flight counter alone, which is blind to what the engine
// itself reports: a worker whose KV cache is full queues the next request
// and answers late, while a neighbour with the same in-flight count has
// room. The workers' heartbeats already carry those samples (build item 14,
// /loadz); this file lets pick() read them.
//
//   score(worker) = in-flight + queue depth + kvWeight × kvUsedPct / 100
//
// Lowest score wins. A worker at or above the saturation threshold is placed
// behind every worker with headroom and only receives a request when none
// has any — it stops receiving new requests BEFORE it errors. Both knobs
// arrive in the policy snapshot the control plane pushes (ADR-001: the CP
// pushes policy, the leader decides per request); both default off, so a
// leader with no policy orders exactly as before.
//
// Prefix affinity: requests that share a prompt prefix land on the worker
// that last served the longest part of it, whose prefix cache likely still
// holds it — an approximate scorer, for engines that publish no KV events
// (llama.cpp); a worker's reported blocks override it (pick.go 3b). A bounded
// table with the sticky TTL, keyed by a chain of hashes over the prompt's
// leading chunks — never the prompt itself.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"
	"hash/maphash"
	"time"

	"github.com/opod-io/opod-core/internal/engines"
)

// LoadSignal is what the picker reads about one worker: the engine's own
// KV-cache use and queue depth from its last heartbeat. ok=false means no
// fresh sample (the worker is scored on in-flight alone).
type LoadSignal struct {
	KVUsedPct  float64
	QueueDepth int64
	// SampledAt is when the engine was scraped. Set on a pulled sample
	// (ADR-109): requests sent to the worker after it count toward its rank,
	// since its queue figure cannot hold them yet. Zero = not dated.
	SampledAt time.Time
}

// sentKeep bounds the per-worker record of send times: the last this many,
// none older than sentWindow. A sample is a few hundred ms old, so ten
// seconds is far past any one the rank reads.
const (
	sentKeep   = 64
	sentWindow = 10 * time.Second
)

// noteSentLocked records that a request was sent to node at now. Caller
// holds r.mu.
func (r *Router) noteSentLocked(node string, now time.Time) {
	ts := append(r.sent[node], now)
	cut := 0
	for cut < len(ts) && (len(ts)-cut > sentKeep || now.Sub(ts[cut]) > sentWindow) {
		cut++
	}
	r.sent[node] = ts[cut:]
}

// sentSinceLocked counts the requests sent to node after at. Caller holds
// r.mu (read).
func (r *Router) sentSinceLocked(node string, at time.Time) int {
	n := 0
	for i := len(r.sent[node]) - 1; i >= 0 && r.sent[node][i].After(at); i-- {
		n++
	}
	return n
}

// LoadSource answers the current signal for a worker node.
type LoadSource func(nodeID string) (LoadSignal, bool)

// SlotSource answers how many requests a worker (its node id) or a gang
// (GangKey) serves at once; ok=false = it reports none (ADR-091).
type SlotSource func(key string) (int, bool)

// SetSlotSource wires the leader's slot counts into the picker: a worker
// whose requests in flight reach its slots ranks behind every worker with a
// free slot, and a sticky or prefix pin never lands a request on it while
// another has one (ADR-091). The leader's gate admits against the pool's
// total; this is what lands each admitted request on a free slot.
func (r *Router) SetSlotSource(fn SlotSource) {
	r.mu.Lock()
	r.slotSource = fn
	r.mu.Unlock()
}

// slotsFull reports whether key's requests in flight have reached the slots
// it reports. Caller holds r.mu (read). False when it reports none.
func (r *Router) slotsFull(key string) bool {
	if r.slotSource == nil {
		return false
	}
	n, ok := r.slotSource(key)
	return ok && n > 0 && r.inflight[key] >= n
}

// slotsFullLocked is slotsFull for a caller that does not hold r.mu.
func (r *Router) slotsFullLocked(key string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.slotsFull(key)
}

// SetLoadSource wires the leader's heartbeat samples into the picker.
func (r *Router) SetLoadSource(fn LoadSource) {
	r.mu.Lock()
	r.loadSource = fn
	r.mu.Unlock()
}

// SetLoadAware applies the policy's routing parameters. kvWeight ≤ 0 scores
// on in-flight + queue only; saturationPct ≤ 0 turns the saturation rule
// off; prefixAffinity switches the prefix pins on (they need a sticky TTL to
// expire by — without one a 10-minute default applies).
func (r *Router) SetLoadAware(kvWeight float64, saturationPct int, prefixAffinity bool) {
	r.mu.Lock()
	if kvWeight < 0 {
		kvWeight = 0
	}
	if saturationPct < 0 || saturationPct > 100 {
		saturationPct = 0
	}
	r.kvWeight, r.kvSaturationPct, r.prefixAffinity = kvWeight, saturationPct, prefixAffinity
	if prefixAffinity && r.prefixPins == nil {
		r.prefixPins = make(map[string]stickyEntry)
	}
	r.mu.Unlock()
}

// LoadAware reports the applied parameters (for /admin/v1 and tests).
func (r *Router) LoadAware() (kvWeight float64, saturationPct int, prefixAffinity bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.kvWeight, r.kvSaturationPct, r.prefixAffinity
}

// loadRank is one worker's place in the pick order (candRank on the worker):
// saturated workers sort after every unsaturated one, then by score. Caller
// holds r.mu (read).
func (r *Router) loadRank(nodeID string) (saturated bool, score float64) {
	return r.candRank(workerCandidate(nodeID, false))
}

// ---- prefix affinity -------------------------------------------------------

type prefixKeyCtx struct{}

const (
	// prefixChunkChars is one dense link of the prefix chain: a request's
	// leading text is hashed in chunks of this many bytes, each hash covering
	// every chunk before it, so two prompts share keys for exactly as long as
	// their text does — a shared system prompt matches with a different
	// question after it. It stands in for an engine's KV block, which the
	// router cannot see without the engine's events (prefixblocks.go).
	prefixChunkChars = 256
	// prefixMaxChunks is the dense head: every link to 4 KiB.
	prefixMaxChunks = 16
	// prefixPinsMax bounds the pin table. Every served request remembers up
	// to prefixMaxChunks keys plus one per sparse link; past the bound the
	// expired go first, then arbitrary ones — a pin is a guess, and a lost
	// guess routes by load.
	prefixPinsMax = 1 << 17

	// DefaultPrefixDepthKiB is how much of a prompt the prefix chain matches
	// when the policy does not say (routing.prefixDepthKiB 0, PLAN T28.11):
	// an agent's system prompt and tool list pass 4 KiB on their own, and the
	// conversation behind them is where prefill costs. MaxPrefixDepthKiB
	// bounds what a policy can ask: 1 MiB of text, about 256 k tokens.
	DefaultPrefixDepthKiB = 256
	MaxPrefixDepthKiB     = 1024
)

// PrefixDepthKiB normalises a policy's routing.prefixDepthKiB: 0 or less is
// DefaultPrefixDepthKiB, and more than MaxPrefixDepthKiB is that bound.
func PrefixDepthKiB(v int) int {
	switch {
	case v <= 0:
		return DefaultPrefixDepthKiB
	case v > MaxPrefixDepthKiB:
		return MaxPrefixDepthKiB
	}
	return v
}

// SetPrefixDepth applies the policy's routing.prefixDepthKiB (PrefixDepthKiB
// normalises it).
func (r *Router) SetPrefixDepth(kib int) {
	r.mu.Lock()
	r.prefixDepthKiB = PrefixDepthKiB(kib)
	r.mu.Unlock()
}

// PrefixDepth is the depth in force, in KiB: the prefix chain's cap, and the
// leader's block index tokenizes as deep (leader/prefixindex.go).
func (r *Router) PrefixDepth() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return PrefixDepthKiB(r.prefixDepthKiB)
}

// prefixTailSeed keys the sparse tail's hash. The pins live in this process
// only — nothing writes a key out or compares one with another process — so a
// seed drawn at start is enough.
var prefixTailSeed = maphash.MakeSeed()

// prefixKeysOf hashes the request's leading text — the system prompt, then
// every turn in order, each as role, a zero byte, content, a zero byte — as a
// chain, shortest prefix first (T19.23), to depthKiB KiB (PrefixDepthKiB).
// Never the prompt itself: each key is 8 bytes of a hash. Nil when there is
// nothing to key on.
//
// The chain is dense, then sparse (T28.11). Every prefixChunkChars link to
// 4 KiB, each a SHA-256 over the link before it and its chunk; a final
// partial chunk is a link too, so a short prompt still has a key. Past 4 KiB,
// one link at each doubling distance — 8, 16, 32 … KiB — and at the cap,
// each a 64-bit maphash over the link before it and every byte since: a pin
// per 256 bytes of a 1 MB prompt would fill the pin table (prefixPinsMax),
// and a match at the deepest common sparse link is within 2× of the true
// common prefix, which is enough to rank. SHA-256 over a 256 KiB tail
// measured 160 µs on an Apple M3, maphash 31 µs, against a 50 µs budget per
// request. Nothing past the cap is read, so a 1 MB prompt costs its cap.
// Depth 4 is the dense head alone: the keys as they were before T28.11, byte
// for byte.
//
// The request's cache salt seeds the chain (T26.8): the same text under two
// salts is two chains, as it is two prefixes in an engine that keys its cache
// by the salt. With no salt the chain is exactly what it was before salts.
func prefixKeysOf(req engines.ChatRequest, depthKiB int) []string {
	k := prefixKeyer{limit: PrefixDepthKiB(depthKiB) << 10}
	k.dense = min(k.limit, prefixChunkChars*prefixMaxChunks)
	k.next = 2 * prefixChunkChars * prefixMaxChunks
	if req.CacheSalt != "" {
		k.prev = sha256.Sum256([]byte("opod-cache-salt\x00" + req.CacheSalt))
	}
	k.sha = sha256.New()
	if req.System != "" {
		k.part("system", req.System)
	}
	for _, m := range req.Messages {
		if m.Content != "" {
			k.part(m.Role, m.Content)
		}
	}
	return k.finish()
}

// prefixKeyer is prefixKeysOf's state: the stream of the prompt's bytes is
// written through it once, and a key leaves at each link.
type prefixKeyer struct {
	limit, dense int // bytes the chain covers in all, and densely
	off          int // bytes of the stream written so far
	next         int // the next sparse link's offset
	prev         [sha256.Size]byte
	sha          hash.Hash
	chunk        [prefixChunkChars]byte
	n            int // bytes in chunk
	tail         maphash.Hash
	keys         []string
}

func (k *prefixKeyer) part(role, content string) {
	k.write(role)
	k.write("\x00")
	k.write(content)
	k.write("\x00")
}

func (k *prefixKeyer) write(s string) {
	for len(s) > 0 && k.off < k.limit {
		if k.off < k.dense {
			c := copy(k.chunk[k.n:], s[:min(len(s), k.dense-k.off)])
			k.n += c
			k.off += c
			s = s[c:]
			if k.n == prefixChunkChars {
				k.denseLink()
			}
			continue
		}
		if k.off == k.dense {
			k.tail.SetSeed(prefixTailSeed)
			_, _ = k.tail.Write(k.prev[:]) // the tail continues the dense chain
		}
		stop := min(k.next, k.limit)
		c := min(len(s), stop-k.off)
		_, _ = k.tail.WriteString(s[:c])
		k.off += c
		s = s[c:]
		if k.off == stop {
			k.sparseLink()
		}
	}
}

// denseLink closes the chunk in hand: SHA-256 over the link before it and
// its bytes.
func (k *prefixKeyer) denseLink() {
	k.sha.Reset()
	k.sha.Write(k.prev[:])
	k.sha.Write(k.chunk[:k.n])
	k.sha.Sum(k.prev[:0])
	k.keys = append(k.keys, hex.EncodeToString(k.prev[:8]))
	k.n = 0
}

// sparseLink keys every byte to here; the next link is twice as deep.
func (k *prefixKeyer) sparseLink() {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], k.tail.Sum64())
	k.keys = append(k.keys, hex.EncodeToString(b[:]))
	k.next *= 2
}

func (k *prefixKeyer) finish() []string {
	if k.n > 0 {
		k.denseLink() // a short prompt's partial chunk
	}
	return k.keys
}

// withPrefixKeys attaches the request's prefix chain for pick() to read.
func withPrefixKeys(ctx context.Context, keys []string) context.Context {
	if len(keys) == 0 {
		return ctx
	}
	return context.WithValue(ctx, prefixKeyCtx{}, keys)
}

func prefixKeysFrom(ctx context.Context) []string {
	k, _ := ctx.Value(prefixKeyCtx{}).([]string)
	return k
}

const defaultPrefixTTL = 10 * time.Minute

func (r *Router) prefixTTL() time.Duration {
	if ttl := r.StickyTTL(); ttl > 0 {
		return ttl
	}
	return defaultPrefixTTL
}

// prefixPick returns the worker pinned to the longest link of the request's
// prefix chain that usable accepts, or "" (affinity off, no key, no live pin).
// usable is how pick() keeps a pin off a worker that would queue the request
// (T10.15); a shorter link may then name another worker that served the same
// lead. Expired pins are left to the sweep in rememberPrefix.
func (r *Router) prefixPick(ctx context.Context, model string, usable func(nodeID string) bool) string {
	keys := prefixKeysFrom(ctx)
	if len(keys) == 0 || model == "" {
		return ""
	}
	now := time.Now()
	var pinned []string
	r.mu.RLock()
	if r.prefixAffinity && r.prefixPins != nil {
		for i := len(keys) - 1; i >= 0; i-- {
			if e, ok := r.prefixPins[keys[i]+"|"+model]; ok && now.Before(e.ExpiresAt) {
				pinned = append(pinned, e.NodeID)
			}
		}
	}
	r.mu.RUnlock()
	for _, n := range pinned {
		if usable == nil || usable(n) {
			return n
		}
	}
	return ""
}

// rememberPrefix pins every link of the prefix chain to the worker that just
// served it. Called on the success path beside rememberSticky. The table is
// swept like the sticky one, and bounded by prefixPinsMax.
func (r *Router) rememberPrefix(ctx context.Context, model, nodeID string) {
	keys := prefixKeysFrom(ctx)
	if len(keys) == 0 || model == "" || nodeID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.prefixAffinity || r.prefixPins == nil {
		return
	}
	exp := time.Now().Add(r.prefixTTL())
	for _, k := range keys {
		r.prefixPins[k+"|"+model] = stickyEntry{NodeID: nodeID, ExpiresAt: exp}
	}
	r.prefixInserts++
	if r.prefixInserts%stickySweepEvery == 0 || len(r.prefixPins) > prefixPinsMax {
		now := time.Now()
		for k, e := range r.prefixPins {
			if now.After(e.ExpiresAt) {
				delete(r.prefixPins, k)
			}
		}
		if len(r.prefixPins) > prefixPinsMax {
			for k := range r.prefixPins { // map order: arbitrary victims
				if len(r.prefixPins) <= prefixPinsMax*3/4 {
					break
				}
				delete(r.prefixPins, k)
			}
		}
	}
}
