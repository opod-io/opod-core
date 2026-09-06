package router

import (
	"testing"
	"time"
)

func newSortTestRouter() *Router {
	r := New(nil, nil)
	r.SetPriceResolver(func(m string) float64 {
		switch m {
		case "free-local":
			return 0
		case "cheap-vendor":
			return 0.5
		case "pricey-vendor":
			return 18.0
		}
		return 0
	})
	return r
}

func TestSortChainPrice(t *testing.T) {
	r := newSortTestRouter()
	got := r.sortChain([]string{"pricey-vendor", "free-local", "cheap-vendor"}, SortPrice)
	want := []string{"free-local", "cheap-vendor", "pricey-vendor"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("price sort = %v, want %v", got, want)
		}
	}
	// Without a resolver the chain is untouched.
	r2 := New(nil, nil)
	got2 := r2.sortChain([]string{"b", "a"}, SortPrice)
	if got2[0] != "b" {
		t.Errorf("price sort without resolver must preserve order, got %v", got2)
	}
}

func TestSortChainPriceStableOnTies(t *testing.T) {
	r := newSortTestRouter()
	// Both free → catalog preference order must survive.
	got := r.sortChain([]string{"free-local", "also-free", "cheap-vendor"}, SortPrice)
	if got[0] != "free-local" || got[1] != "also-free" {
		t.Errorf("tie order not stable: %v", got)
	}
}

func TestSortChainLatency(t *testing.T) {
	r := newSortTestRouter()
	for i := 0; i < 10; i++ {
		r.latency.record("slow", 5*time.Second, 0)
		r.latency.record("fast", 50*time.Millisecond, 0)
	}
	got := r.sortChain([]string{"slow", "unsampled", "fast"}, SortLatency)
	if got[0] != "fast" || got[1] != "slow" || got[2] != "unsampled" {
		t.Errorf("latency sort = %v, want [fast slow unsampled] (no samples = last)", got)
	}
}

func TestSortChainThroughput(t *testing.T) {
	r := newSortTestRouter()
	for i := 0; i < 10; i++ {
		r.latency.record("slow-gen", time.Second, 10)  // 10 tok/s
		r.latency.record("fast-gen", time.Second, 120) // 120 tok/s
	}
	got := r.sortChain([]string{"slow-gen", "fast-gen", "unsampled"}, SortThroughput)
	if got[0] != "fast-gen" || got[1] != "slow-gen" || got[2] != "unsampled" {
		t.Errorf("throughput sort = %v, want [fast-gen slow-gen unsampled]", got)
	}
}

func TestThroughputNeedsFiveSamples(t *testing.T) {
	r := newSortTestRouter()
	for i := 0; i < 4; i++ {
		r.latency.record("m", time.Second, 100)
	}
	if tps := r.latency.throughput("m"); tps != 0 {
		t.Errorf("throughput with 4 samples = %v, want 0 (too few to rank)", tps)
	}
	r.latency.record("m", time.Second, 100)
	if tps := r.latency.throughput("m"); tps < 99 || tps > 101 {
		t.Errorf("throughput with 5 samples = %v, want ≈100 tok/s", tps)
	}
}

func TestOverridesSortClamp(t *testing.T) {
	if got := (Overrides{Sort: "cheapest"}).Clamp().Sort; got != "" {
		t.Errorf("unknown sort must clamp to empty, got %q", got)
	}
	if got := (Overrides{Sort: SortPrice}).Clamp().Sort; got != SortPrice {
		t.Errorf("valid sort must survive Clamp, got %q", got)
	}
	if !(Overrides{Sort: SortLatency}).IsSet() {
		t.Errorf("Sort alone must make IsSet true")
	}
}
