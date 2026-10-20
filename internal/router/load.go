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
	"encoding/hex"
	"time"

	"github.com/opod-io/opod/internal/engines"
)

// LoadSignal is what the picker reads about one worker: the engine's own
// KV-cache use and queue depth from its last heartbeat. ok=false means no
// fresh sample (the worker is scored on in-flight alone).
type LoadSignal struct {
	KVUsedPct  float64
	QueueDepth int64
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

// loadRank is one worker's place in the pick order: saturated workers sort
// after every unsaturated one, then by score. Caller holds r.mu (read).
func (r *Router) loadRank(nodeID string) (saturated bool, score float64) {
	score = float64(r.inflight[nodeID])
	if r.slotsFull(nodeID) {
		return true, score // every slot busy: a request here waits in the engine's queue
	}
	if r.loadSource == nil {
		return false, score
	}
	sig, ok := r.loadSource(nodeID)
	if !ok {
		return false, score
	}
	score += float64(sig.QueueDepth)
	if r.kvWeight > 0 {
		score += r.kvWeight * sig.KVUsedPct / 100
	}
	saturated = r.kvSaturationPct > 0 && sig.KVUsedPct >= float64(r.kvSaturationPct)
	return saturated, score
}

// ---- prefix affinity -------------------------------------------------------

type prefixKeyCtx struct{}

const (
	// prefixChunkChars is one link of the prefix chain: a request's leading
	// text is hashed in chunks of this many bytes, each hash covering every
	// chunk before it, so two prompts share keys for exactly as long as their
	// text does — a shared system prompt matches with a different question
	// after it. It stands in for an engine's KV block, which the router cannot
	// see without the engine's events (prefixblocks.go).
	prefixChunkChars = 256
	// prefixMaxChunks bounds the chain: the leading 4 KiB decide a match, so
	// a long document never costs more than its head.
	prefixMaxChunks = 16
	// prefixPinsMax bounds the pin table. Every served request remembers up
	// to prefixMaxChunks keys; past the bound the expired go first, then
	// arbitrary ones — a pin is a guess, and a lost guess routes by load.
	prefixPinsMax = 1 << 17
)

// prefixKeysOf hashes the request's leading text — the system prompt, then
// every turn in order — as a chain of prefixChunkChars chunks, shortest
// prefix first (T19.23). A final partial chunk is a link too, so a short
// prompt still has a key. Never the prompt itself: each key is 8 bytes of a
// SHA-256. Nil when there is nothing to key on.
func prefixKeysOf(req engines.ChatRequest) []string {
	const capChars = prefixChunkChars * prefixMaxChunks
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
	keys := make([]string, 0, (len(head)+prefixChunkChars-1)/prefixChunkChars)
	var prev [sha256.Size]byte
	for off := 0; off < len(head); off += prefixChunkChars {
		h := sha256.New()
		h.Write(prev[:])
		h.Write(head[off:min(off+prefixChunkChars, len(head))])
		copy(prev[:], h.Sum(nil))
		keys = append(keys, hex.EncodeToString(prev[:8]))
	}
	return keys
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
	if r.stickyTTL > 0 {
		return r.stickyTTL
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
