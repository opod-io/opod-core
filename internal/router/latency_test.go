package router

import (
	"testing"
	"time"
)

func TestLatencyStats_P95InsufficientSamples(t *testing.T) {
	s := newLatencyStats(LatencyConfig{P95Threshold: time.Second})
	for i := 0; i < 4; i++ {
		s.record("m1", 10*time.Second, 0)
	}
	if got := s.p95("m1"); got != 0 {
		t.Errorf("p95 with 4 samples should be 0 (need ≥5); got %v", got)
	}
}

func TestLatencyStats_P95Computation(t *testing.T) {
	s := newLatencyStats(LatencyConfig{P95Threshold: time.Second})
	// 20 samples: 19 × 1ms + 1 × 100ms. By nearest-rank, p95 of 20
	// samples = ceil(0.95 * 20) = 19th value (1-indexed) = the 19th
	// smallest = 1ms. That's standard p95 — it's NOT supposed to
	// catch the single outlier (which would be p100/max). p95
	// flags "the slowest 5% are this slow or worse," and with 20
	// samples that's the 19th value.
	for i := 0; i < 19; i++ {
		s.record("m1", time.Millisecond, 0)
	}
	s.record("m1", 100*time.Millisecond, 0)
	if got := s.p95("m1"); got != time.Millisecond {
		t.Errorf("p95 of 19×1ms + 1×100ms = %v, want 1ms (the 19th-smallest)", got)
	}

	// More meaningful: 100 samples with 5 outliers — p95 should land in
	// the outlier band.
	s2 := newLatencyStats(LatencyConfig{})
	for i := 0; i < 95; i++ {
		s2.record("m2", time.Millisecond, 0)
	}
	for i := 0; i < 5; i++ {
		s2.record("m2", 100*time.Millisecond, 0)
	}
	if got := s2.p95("m2"); got != 100*time.Millisecond {
		t.Errorf("p95 of 95×1ms + 5×100ms = %v, want 100ms", got)
	}
}

func TestLatencyStats_ReorderDisabledWhenThresholdZero(t *testing.T) {
	s := newLatencyStats(LatencyConfig{}) // P95Threshold = 0 → disabled
	for i := 0; i < 10; i++ {
		s.record("slow", 10*time.Second, 0)
	}
	chain := []string{"slow", "fast"}
	reordered, swapped := s.reorderByLatency(chain)
	if swapped {
		t.Error("expected no swap when threshold is 0")
	}
	if reordered[0] != "slow" {
		t.Error("chain should be unchanged")
	}
}

func TestLatencyStats_ReorderWhenPrimarySlow(t *testing.T) {
	s := newLatencyStats(LatencyConfig{P95Threshold: 2 * time.Second})
	// Primary "slow" has p95 = 10s (above threshold)
	for i := 0; i < 10; i++ {
		s.record("slow", 10*time.Second, 0)
	}
	// Fallback "fast" has p95 = 50ms (below threshold)
	for i := 0; i < 10; i++ {
		s.record("fast", 50*time.Millisecond, 0)
	}
	chain := []string{"slow", "fast"}
	reordered, swapped := s.reorderByLatency(chain)
	if !swapped {
		t.Fatal("expected swap when primary p95 > threshold and fallback is faster")
	}
	if reordered[0] != "fast" {
		t.Errorf("front = %q, want fast", reordered[0])
	}
	if reordered[1] != "slow" {
		t.Errorf("slow should drop to position 1, got chain %v", reordered)
	}
}

func TestLatencyStats_NoReorderWhenFallbackUnseen(t *testing.T) {
	// Primary slow, but no samples for the fallback yet. We can't compare,
	// so the chain stays as-is.
	s := newLatencyStats(LatencyConfig{P95Threshold: time.Second})
	for i := 0; i < 10; i++ {
		s.record("slow", 5*time.Second, 0)
	}
	chain := []string{"slow", "untested"}
	_, swapped := s.reorderByLatency(chain)
	if swapped {
		t.Error("should not reorder when no fallback has been sampled")
	}
}

func TestLatencyStats_RingBufferBounded(t *testing.T) {
	s := newLatencyStats(LatencyConfig{Window: 3, P95Threshold: time.Second})
	for i := 0; i < 100; i++ {
		s.record("m1", time.Duration(i)*time.Millisecond, 0)
	}
	s.mu.RLock()
	rb := s.samples["m1"]
	s.mu.RUnlock()
	if rb == nil {
		t.Fatal("expected ring buffer to be allocated")
	}
	got := len(rb.snapshot())
	if got != 3 {
		t.Errorf("window=3 should cap samples at 3, got %d", got)
	}
}

func TestRingBuf_WrapsAndKeepsOrder(t *testing.T) {
	rb := newRingBuf(3)
	for _, ms := range []int{1, 2, 3, 4, 5} {
		rb.push(time.Duration(ms) * time.Millisecond)
	}
	snap := rb.snapshot()
	if len(snap) != 3 {
		t.Fatalf("want 3 samples, got %d", len(snap))
	}
	// After 5 pushes into a 3-slot ring, expect the last three: 3,4,5
	// in chronological (oldest-first) order.
	want := []time.Duration{3 * time.Millisecond, 4 * time.Millisecond, 5 * time.Millisecond}
	for i, w := range want {
		if snap[i] != w {
			t.Errorf("position %d: want %v, got %v", i, w, snap[i])
		}
	}
}
