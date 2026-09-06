package router

import (
	"context"
	"testing"
	"time"
)

// TestOverrides_Clamp pins the documented caps. The HTTP layer hands
// these to the router straight from client input, so a misconfigured
// client must not be able to spin up an arbitrary retry storm.
func TestOverrides_Clamp(t *testing.T) {
	cases := []struct {
		name        string
		in          Overrides
		wantRetries int
		wantBackoff int
	}{
		{"all zeros", Overrides{}, 0, 0},
		{"in range", Overrides{NumRetries: 3, RetryBackoffMS: 250}, 3, 250},
		{"retries clamped to MaxRetries", Overrides{NumRetries: 99}, MaxRetries, 0},
		{"backoff clamped to cap", Overrides{RetryBackoffMS: 60_000}, 0, RetryBackoffCapMS},
		{"negative floors", Overrides{NumRetries: -1, RetryBackoffMS: -50}, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.in.Clamp()
			if got.NumRetries != c.wantRetries {
				t.Errorf("NumRetries = %d, want %d", got.NumRetries, c.wantRetries)
			}
			if got.RetryBackoffMS != c.wantBackoff {
				t.Errorf("RetryBackoffMS = %d, want %d", got.RetryBackoffMS, c.wantBackoff)
			}
		})
	}
}

// TestOverrides_ContextRoundtrip ensures the ctx helpers actually
// stash and retrieve the struct (typo-resistant — would catch a
// renamed key in either direction).
func TestOverrides_ContextRoundtrip(t *testing.T) {
	o := Overrides{Fallbacks: []string{"a", "b"}, NumRetries: 2, RetryBackoffMS: 100}
	ctx := WithOverrides(context.Background(), o)
	got := FromContext(ctx)
	if got.NumRetries != 2 || got.RetryBackoffMS != 100 || len(got.Fallbacks) != 2 || got.Fallbacks[0] != "a" {
		t.Fatalf("roundtrip failed: %#v", got)
	}
}

// TestOverrides_IsSet covers the zero-value short-circuit the handlers
// rely on to avoid attaching overrides to ctx for plain requests.
func TestOverrides_IsSet(t *testing.T) {
	if (Overrides{}).IsSet() {
		t.Error("zero value should not be set")
	}
	if !(Overrides{Fallbacks: []string{"x"}}).IsSet() {
		t.Error("fallback list should be set")
	}
	if !(Overrides{NumRetries: 1}).IsSet() {
		t.Error("retries > 0 should be set")
	}
	if !(Overrides{Hedge: true}).IsSet() {
		t.Error("hedge true should be set")
	}
	if (Overrides{RetryBackoffMS: 100}).IsSet() {
		t.Error("backoff alone is not 'set' — only retries/fallbacks/hedge turn the path on")
	}
}

// TestSetHedgeReplicas_Clamps verifies the documented caps.
func TestSetHedgeReplicas_Clamps(t *testing.T) {
	r := &Router{}
	r.SetHedgeReplicas(0)
	if r.hedgeReplicas != 0 {
		t.Errorf("0 → disable, got %d", r.hedgeReplicas)
	}
	r.SetHedgeReplicas(1)
	if r.hedgeReplicas != 0 {
		t.Errorf("1 → disable (one replica is not hedging), got %d", r.hedgeReplicas)
	}
	r.SetHedgeReplicas(2)
	if r.hedgeReplicas != 2 {
		t.Errorf("2 → 2, got %d", r.hedgeReplicas)
	}
	r.SetHedgeReplicas(100)
	if r.hedgeReplicas != MaxHedgeReplicas {
		t.Errorf("100 → clamp to %d, got %d", MaxHedgeReplicas, r.hedgeReplicas)
	}
}

// TestChainFor_PerRequestOverridesCatalog verifies overrides win over
// the catalog resolver. This is the bit operators are paying for.
func TestChainFor_PerRequestOverridesCatalog(t *testing.T) {
	r := &Router{}
	r.SetFallbackResolver(func(string) FallbackChains {
		return FallbackChains{Generic: []string{"catalog-1", "catalog-2"}}
	})

	t.Run("no overrides uses catalog", func(t *testing.T) {
		chain, source, _ := r.chainFor("primary", Overrides{})
		if source != "catalog" {
			t.Errorf("source = %q, want catalog", source)
		}
		if len(chain) != 3 || chain[0] != "primary" || chain[1] != "catalog-1" {
			t.Errorf("chain = %v", chain)
		}
	})
	t.Run("override replaces catalog", func(t *testing.T) {
		chain, source, _ := r.chainFor("primary", Overrides{Fallbacks: []string{"req-1", "req-2", "req-3"}})
		if source != "request" {
			t.Errorf("source = %q, want request", source)
		}
		if len(chain) != 4 || chain[0] != "primary" || chain[1] != "req-1" || chain[3] != "req-3" {
			t.Errorf("chain = %v", chain)
		}
	})
}

// TestWaitBackoff_Doubling sanity-checks the exponential schedule plus
// the cap. Using small numbers (10 ms initial) keeps the test fast.
func TestWaitBackoff_Doubling(t *testing.T) {
	cases := []struct {
		retry     int
		initialMS int
		wantMinMS int
		wantMaxMS int
	}{
		{retry: 1, initialMS: 10, wantMinMS: 10, wantMaxMS: 30},
		{retry: 2, initialMS: 10, wantMinMS: 20, wantMaxMS: 40},
		{retry: 3, initialMS: 10, wantMinMS: 40, wantMaxMS: 60},
	}
	for _, c := range cases {
		start := time.Now()
		if err := waitBackoff(context.Background(), c.retry, c.initialMS); err != nil {
			t.Fatalf("waitBackoff: %v", err)
		}
		dur := time.Since(start)
		if dur < time.Duration(c.wantMinMS)*time.Millisecond {
			t.Errorf("retry=%d initial=%d ms: dur=%v too short (want >= %dms)", c.retry, c.initialMS, dur, c.wantMinMS)
		}
		if dur > time.Duration(c.wantMaxMS)*time.Millisecond {
			t.Errorf("retry=%d initial=%d ms: dur=%v too long (want <= %dms)", c.retry, c.initialMS, dur, c.wantMaxMS)
		}
	}
}

// TestWaitBackoff_CapHonored ensures the doubling plateaus at the cap
// even if the retry count would push it past.
func TestWaitBackoff_CapHonored(t *testing.T) {
	// retry=10 with initial=2000ms would double to 1024s without the cap.
	// We pick a smaller cap via setting a tighter ceiling so the test stays fast.
	start := time.Now()
	err := waitBackoff(context.Background(), 4, 2000)
	if err != nil {
		t.Fatalf("waitBackoff: %v", err)
	}
	dur := time.Since(start)
	if dur > time.Duration(RetryBackoffCapMS+200)*time.Millisecond {
		t.Errorf("expected dur <= %dms (cap + jitter), got %v", RetryBackoffCapMS+200, dur)
	}
}

// TestWaitBackoff_CtxCancel proves a cancelled caller releases the
// timer rather than burning the full backoff.
func TestWaitBackoff_CtxCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := waitBackoff(ctx, 1, 1000) // would otherwise wait 1 s
	dur := time.Since(start)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if dur > 100*time.Millisecond {
		t.Errorf("expected fast cancel, got %v", dur)
	}
}
