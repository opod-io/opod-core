package router

import (
	"testing"
	"time"
)

// TestCooldown_DisabledByDefault — the feature must be opt-in. Zero
// values for either knob mean "no cooldowns ever happen, behave like
// before".
func TestCooldown_DisabledByDefault(t *testing.T) {
	r := &Router{
		cooldowns: map[string]time.Time{},
		failures:  map[string]int{},
	}
	for i := 0; i < 10; i++ {
		r.recordOutcome("worker-A", false)
	}
	if r.inCooldown("worker-A") {
		t.Fatal("cooldown should not trigger when not configured")
	}
}

// TestCooldown_TriggersAfterAllowedFails — once configured, N
// consecutive failures parks the node.
func TestCooldown_TriggersAfterAllowedFails(t *testing.T) {
	r := &Router{
		cooldowns: map[string]time.Time{},
		failures:  map[string]int{},
	}
	r.SetPlacementCooldown(3, 50*time.Millisecond)

	r.recordOutcome("worker-A", false)
	r.recordOutcome("worker-A", false)
	if r.inCooldown("worker-A") {
		t.Fatal("should still be eligible after 2/3 failures")
	}
	r.recordOutcome("worker-A", false)
	if !r.inCooldown("worker-A") {
		t.Fatal("3 consecutive failures should park the node")
	}
}

// TestCooldown_SuccessResetsCounter — a success before reaching the
// threshold clears the strikes.
func TestCooldown_SuccessResetsCounter(t *testing.T) {
	r := &Router{
		cooldowns: map[string]time.Time{},
		failures:  map[string]int{},
	}
	r.SetPlacementCooldown(3, 50*time.Millisecond)

	r.recordOutcome("worker-A", false)
	r.recordOutcome("worker-A", false)
	r.recordOutcome("worker-A", true) // resets
	r.recordOutcome("worker-A", false)
	if r.inCooldown("worker-A") {
		t.Fatal("counter should have reset; still only 1 strike since success")
	}
}

// TestCooldown_ExpiresOnTime — after the cooldown duration the node
// re-enters rotation.
func TestCooldown_ExpiresOnTime(t *testing.T) {
	r := &Router{
		cooldowns: map[string]time.Time{},
		failures:  map[string]int{},
	}
	r.SetPlacementCooldown(2, 30*time.Millisecond)

	r.recordOutcome("worker-A", false)
	r.recordOutcome("worker-A", false)
	if !r.inCooldown("worker-A") {
		t.Fatal("should be in cooldown")
	}
	time.Sleep(50 * time.Millisecond)
	if r.inCooldown("worker-A") {
		t.Fatal("cooldown should have expired")
	}
}

// TestCooldown_LocalNodeIsExempt — a flaky local engine is a separate
// problem; record-outcome must not park `local` regardless of failures.
func TestCooldown_LocalNodeIsExempt(t *testing.T) {
	r := &Router{
		localNode: "local",
		cooldowns: map[string]time.Time{},
		failures:  map[string]int{},
	}
	r.SetPlacementCooldown(2, time.Second)

	r.recordOutcome("local", false)
	r.recordOutcome("local", false)
	r.recordOutcome("local", false)
	if r.inCooldown("local") {
		t.Fatal("local node must never enter cooldown")
	}
}

// TestCooldown_ShardKeyIsExempt — shard pseudo-nodes (shard:<model>)
// route to a coordinator; cooldown semantics don't apply.
func TestCooldown_ShardKeyIsExempt(t *testing.T) {
	r := &Router{
		localNode: "local",
		cooldowns: map[string]time.Time{},
		failures:  map[string]int{},
	}
	r.SetPlacementCooldown(2, time.Second)

	r.recordOutcome("shard:llama-3.3-70b-sharded", false)
	r.recordOutcome("shard:llama-3.3-70b-sharded", false)
	r.recordOutcome("shard:llama-3.3-70b-sharded", false)
	if r.inCooldown("shard:llama-3.3-70b-sharded") {
		t.Fatal("shard pseudo-nodes are exempt from cooldown")
	}
}
