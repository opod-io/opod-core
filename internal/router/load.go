package router

// Load-aware and prefix-affine worker choice (ADR-040 gap G3, ROADMAP R9.4;
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
// Prefix affinity: requests that share a prompt prefix (the system prompt
// and the opening user turn) land on the worker that last served that
// prefix, whose prefix cache still holds it. A bounded table with the
// sticky TTL, keyed by a hash of the prefix — never the prompt itself.

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

// prefixKeyOf hashes the request's prompt prefix: the system prompt and the
// first user turn, capped so a long document does not decide the key (the
// engine's prefix cache is a block cache; the opening blocks are what
// repeat). Empty when there is nothing to key on.
func prefixKeyOf(req engines.ChatRequest) string {
	const capChars = 1024
	var head string
	if req.System != "" {
		head = req.System
	}
	for _, m := range req.Messages {
		if m.Role == "system" && head == "" {
			head = m.Content
			continue
		}
		if m.Role == "user" {
			head += "\n" + m.Content
			break
		}
	}
	if head == "" {
		return ""
	}
	if len(head) > capChars {
		head = head[:capChars]
	}
	sum := sha256.Sum256([]byte(head))
	return hex.EncodeToString(sum[:8])
}

// withPrefixKey attaches the request's prefix key for pick() to read.
func withPrefixKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	return context.WithValue(ctx, prefixKeyCtx{}, key)
}

func prefixKeyFrom(ctx context.Context) string {
	k, _ := ctx.Value(prefixKeyCtx{}).(string)
	return k
}

const defaultPrefixTTL = 10 * time.Minute

func (r *Router) prefixTTL() time.Duration {
	if r.stickyTTL > 0 {
		return r.stickyTTL
	}
	return defaultPrefixTTL
}

// prefixPick returns the worker pinned to the request's prefix for this
// model, or "" (affinity off, no key, no pin, or an expired one).
func (r *Router) prefixPick(ctx context.Context, model string) string {
	key := prefixKeyFrom(ctx)
	r.mu.RLock()
	on := r.prefixAffinity && r.prefixPins != nil
	var e stickyEntry
	var ok bool
	if on && key != "" && model != "" {
		e, ok = r.prefixPins[key+"|"+model]
	}
	r.mu.RUnlock()
	if !ok {
		return ""
	}
	if time.Now().After(e.ExpiresAt) {
		r.mu.Lock()
		if e2, ok := r.prefixPins[key+"|"+model]; ok && time.Now().After(e2.ExpiresAt) {
			delete(r.prefixPins, key+"|"+model)
		}
		r.mu.Unlock()
		return ""
	}
	return e.NodeID
}

// rememberPrefix pins the prefix to the worker that just served it. Called
// on the success path beside rememberSticky. The table is swept like the
// sticky one so one-shot prefixes do not accumulate.
func (r *Router) rememberPrefix(ctx context.Context, model, nodeID string) {
	key := prefixKeyFrom(ctx)
	if key == "" || model == "" || nodeID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.prefixAffinity || r.prefixPins == nil {
		return
	}
	r.prefixPins[key+"|"+model] = stickyEntry{NodeID: nodeID, ExpiresAt: time.Now().Add(r.prefixTTL())}
	r.prefixInserts++
	if r.prefixInserts%stickySweepEvery == 0 || len(r.prefixPins) > stickySweepThreshold {
		now := time.Now()
		for k, e := range r.prefixPins {
			if now.After(e.ExpiresAt) {
				delete(r.prefixPins, k)
			}
		}
	}
}
