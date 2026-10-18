package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// Fail-static: a door keeps enforcing the last thing the brain said, and says
// how stale it is rather than pretending it is current.
func TestAnUnreachableLeaderLeavesTheLastSnapshotEnforced(t *testing.T) {
	ctx := context.Background()
	var down atomic.Bool
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if down.Load() {
			http.Error(w, "nope", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, `{"ts":1,"doors":3,"lag_bound_ms":10000,
		  "keys":[{"api_key_id":"k1","tokens_today":10,"quota_daily_tokens":100,"rpm_limit":90,"rpm_share":30}]}`)
	}))
	defer leader.Close()
	s := NewSpend(leader.URL, "tok")
	clock := time.Now()
	s.now = func() time.Time { return clock }
	if err := s.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	clock = clock.Add(time.Minute)
	if err := s.Poll(ctx); err == nil {
		t.Error("a failed poll must report the failure")
	}
	// Still standing: the last snapshot's door count is kept, and the door can
	// say how stale it is.
	age, bound, doors, lastErr := s.Age()
	if age < time.Minute || bound != 10*time.Second || doors != 3 || lastErr == "" {
		t.Errorf("a door must be able to say how stale it is and why: age=%v bound=%v doors=%d err=%q", age, bound, doors, lastErr)
	}
}

// A door divides a key's per-minute limit by the fronts the leader counts —
// the doors and the leader itself (T19.17) — and holds the count through an
// unreachable leader like the rest of the snapshot.
func TestADoorReadsTheFrontsFromTheSnapshot(t *testing.T) {
	ctx := context.Background()
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"ts":1,"doors":2,"fronts":3,"lag_bound_ms":10000,"day":"2026-10-17","key_tokens_today":{"k_cp_a":120}}`)
	}))
	defer leader.Close()
	s := NewSpend(leader.URL, "tok")
	if s.Fronts() != 0 {
		t.Fatal("no snapshot yet: no count")
	}
	var got Snapshot
	s.OnSnapshot = func(snap Snapshot) { got = snap }
	if err := s.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if s.Fronts() != 3 || got.Day != "2026-10-17" || got.Keys["k_cp_a"] != 120 {
		t.Fatalf("fronts %d, day %q, keys %v", s.Fronts(), got.Day, got.Keys)
	}
}
