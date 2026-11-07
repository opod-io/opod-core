package adminapi

import (
	"encoding/json"
	"strings"
	"testing"
)

// The leader reads the mode under this exact key; a renamed tag would make
// every policy file silently fall back to the default.
func TestPolicyRoutingCacheIsolationWire(t *testing.T) {
	raw, err := json.Marshal(PolicySnapshot{Routing: PolicyRouting{CacheIsolation: CacheIsolationKey}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cacheIsolation":"key"`) {
		t.Fatalf("wire form %s lacks cacheIsolation", raw)
	}
	var back PolicySnapshot
	if err := json.Unmarshal(raw, &back); err != nil || back.Routing.CacheIsolation != CacheIsolationKey {
		t.Fatalf("round trip: %+v %v", back.Routing, err)
	}
	empty, _ := json.Marshal(PolicyRouting{})
	if strings.Contains(string(empty), "cacheIsolation") {
		t.Fatalf("an unset mode must be omitted, got %s", empty)
	}
}

// The leader reads the sticky TTL under this exact key; 0 is omitted, which is
// what tells the leader to keep its own start value.
func TestPolicyRoutingStickyTTLWire(t *testing.T) {
	raw, err := json.Marshal(PolicySnapshot{Routing: PolicyRouting{StickyTTLSeconds: 900}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"stickyTtlSeconds":900`) {
		t.Fatalf("wire form %s lacks stickyTtlSeconds", raw)
	}
	var back PolicySnapshot
	if err := json.Unmarshal(raw, &back); err != nil || back.Routing.StickyTTLSeconds != 900 {
		t.Fatalf("round trip: %+v %v", back.Routing, err)
	}
	empty, _ := json.Marshal(PolicyRouting{})
	if strings.Contains(string(empty), "stickyTtlSeconds") {
		t.Fatalf("an unset TTL must be omitted, got %s", empty)
	}
}

// The tier choice travels under this key, and unset is omitted (= gpu).
func TestPolicyRoutingPrefixBlockTierWire(t *testing.T) {
	raw, _ := json.Marshal(PolicyRouting{PrefixBlockTier: PrefixBlockTierCPULower})
	if !strings.Contains(string(raw), `"prefixBlockTier":"cpu-lower"`) {
		t.Fatalf("wire form %s lacks prefixBlockTier", raw)
	}
	empty, _ := json.Marshal(PolicyRouting{})
	if strings.Contains(string(empty), "prefixBlockTier") {
		t.Fatalf("an unset tier must be omitted, got %s", empty)
	}
}
