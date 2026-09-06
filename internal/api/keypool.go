package api

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// KeyPool holds, per vendor, an ordered set of upstream API keys plus a
// per-key cooldown clock. It lets the egress layer rotate across a user's
// keys for one provider and skip any key that just hit a rate limit (HTTP
// 429) or a transient upstream failure, then bring it back automatically
// once the cooldown elapses.
//
// This mirrors the node circuit-breaker pattern in internal/router (parked
// after consecutive failures, revived after a cooldown) but keyed on
// (vendor, key) rather than (model, node).
//
// The zero value is not usable — construct with NewKeyPool. All methods are
// safe for concurrent use.
type KeyPool struct {
	mu       sync.Mutex
	keys     map[string][]string             // vendor -> ordered keys
	cooldown map[string]map[string]time.Time // vendor -> key -> cooldown-until
	cursor   map[string]int                  // vendor -> round-robin offset
	now      func() time.Time                // injectable clock (tests)
}

// NewKeyPool returns an empty, ready-to-use pool.
func NewKeyPool() *KeyPool {
	return &KeyPool{
		keys:     map[string][]string{},
		cooldown: map[string]map[string]time.Time{},
		cursor:   map[string]int{},
		now:      time.Now,
	}
}

// Set registers the ordered keys for a vendor, replacing any previous set.
// Blank keys are dropped and duplicates are collapsed (first position wins),
// so callers can pass raw env values without pre-cleaning.
func (p *KeyPool) Set(vendor string, keys []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	seen := map[string]bool{}
	clean := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		clean = append(clean, k)
	}
	if len(clean) == 0 {
		delete(p.keys, vendor)
		return
	}
	p.keys[vendor] = clean
}

// HasKeys reports whether the vendor has at least one configured key.
func (p *KeyPool) HasKeys(vendor string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.keys[vendor]) > 0
}

// Candidates returns the vendor's keys in the order the egress layer should
// try them: keys not in cooldown first (rotated so successive requests start
// from a different key and spread load), then — only if every key is cooling
// down — the parked keys ordered by soonest-available. Returns nil when the
// vendor has no keys at all.
//
// Each call advances the per-vendor round-robin cursor by one.
func (p *KeyPool) Candidates(vendor string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	all := p.keys[vendor]
	if len(all) == 0 {
		return nil
	}
	now := p.now()
	off := p.cursor[vendor]
	p.cursor[vendor] = (off + 1) % len(all)
	cd := p.cooldown[vendor]

	live := make([]string, 0, len(all))
	type parkedKey struct {
		key   string
		until time.Time
	}
	var parked []parkedKey
	for i := 0; i < len(all); i++ {
		k := all[(off+i)%len(all)]
		if cd != nil {
			if until, ok := cd[k]; ok && now.Before(until) {
				parked = append(parked, parkedKey{k, until})
				continue
			}
		}
		live = append(live, k)
	}
	if len(live) > 0 {
		return live
	}
	// Everything is cooling down — still return the set (soonest-free
	// first) rather than hard-fail, since the upstream may have recovered
	// before its advertised Retry-After.
	sort.SliceStable(parked, func(i, j int) bool { return parked[i].until.Before(parked[j].until) })
	out := make([]string, len(parked))
	for i, pk := range parked {
		out[i] = pk.key
	}
	return out
}

// Penalize parks a key for d (after a 429 or transient upstream failure). A
// non-positive d falls back to a sane default so callers can pass a parsed
// Retry-After straight through even when it was absent (zero).
func (p *KeyPool) Penalize(vendor, key string, d time.Duration) {
	if d <= 0 {
		d = 30 * time.Second
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cooldown[vendor] == nil {
		p.cooldown[vendor] = map[string]time.Time{}
	}
	p.cooldown[vendor][key] = p.now().Add(d)
}
