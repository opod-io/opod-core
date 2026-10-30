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
