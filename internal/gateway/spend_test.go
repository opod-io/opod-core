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

// The quota a door enforces is the leader's number PLUS what this door has
// served since the snapshot. Without the local half a door would serve the
// whole quota again in every poll window; with it double-counted after a poll,
// it would refuse a key that is nowhere near its limit.
func TestTheQuotaIsTheLeadersNumberPlusWhatThisDoorHasServedSince(t *testing.T) {
	ctx := context.Background()
	var today atomic.Int64
	today.Store(400)
	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"ts":1,"doors":2,"lag_bound_ms":10000,"window_opened_unix":0,
		  "keys":[{"api_key_id":"k1","tokens_today":%d,"quota_daily_tokens":1000,"rpm_limit":60,"tpm_limit":600,"rpm_share":30,"tpm_share":300}]}`, today.Load())
	}))
	defer leader.Close()

	s := NewSpend(leader.URL, "tok")
	// Before any poll: a door that has never heard from the leader does NOT
	// refuse. Falling closed here would take the endpoint down whenever the
	// brain blinks; falling open is the other error and is why the snapshot is
	// polled at all.
	if over, _, _ := s.QuotaExceeded("k1"); over {
		t.Error("an unpolled door must not refuse")
	}
	if err := s.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if over, used, quota := s.QuotaExceeded("k1"); over || used != 400 || quota != 1000 {
		t.Fatalf("after a poll: over=%v used=%d quota=%d", over, used, quota)
	}
	// This door serves 500 more before the next poll. 400 + 500 < 1000.
	s.Spent("k1", 500)
	if over, used, _ := s.QuotaExceeded("k1"); over || used != 900 {
		t.Fatalf("the local half counts: over=%v used=%d want 900", over, used)
	}
	// 100 more crosses it, without waiting for the leader to agree — which is
	// exactly what the 10 s bound buys.
	s.Spent("k1", 100)
	if over, used, _ := s.QuotaExceeded("k1"); !over || used != 1000 {
		t.Fatalf("a door refuses on its own count: over=%v used=%d", over, used)
	}
	// The leader now reports the pushed rows. The local tally must RESET, or
	// the same tokens are counted twice and the key is refused at half its
	// quota.
	today.Store(1000 - 400)
	if err := s.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if over, used, _ := s.QuotaExceeded("k1"); over || used != 600 {
		t.Fatalf("a poll replaces the local tally rather than adding to it: over=%v used=%d want 600", over, used)
	}

	// The share is what the local rate limiter is set to, and it is the
	// leader's, not a fraction this door computed.
	if rpm, tpm := s.Share("k1"); rpm != 30 || tpm != 300 {
		t.Errorf("share = %d/%d, want the leader's 30/300", rpm, tpm)
	}
	if _, _, doors, _ := s.Age(); doors != 2 {
		t.Errorf("the door count is reported so a door can see it is not alone: %d", doors)
	}
	// An unknown key has no ceiling here: the auth snapshot decides whether it
	// exists at all, and inventing a limit for a key the leader did not list
	// would refuse a key that was just created.
	if rpm, tpm := s.Share("nope"); rpm != 0 || tpm != 0 {
		t.Errorf("an unlisted key gets no invented ceiling: %d/%d", rpm, tpm)
	}
	if over, _, _ := s.QuotaExceeded("nope"); over {
		t.Error("an unlisted key is not over a quota it does not have")
	}
}

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
	// Still enforcing: the share is kept, not widened (which would oversell the
	// rate) and not zeroed (which would refuse every request).
	if rpm, _ := s.Share("k1"); rpm != 30 {
		t.Errorf("the last share stands: %d", rpm)
	}
	age, bound, doors, lastErr := s.Age()
	if age < time.Minute || bound != 10*time.Second || doors != 3 || lastErr == "" {
		t.Errorf("a door must be able to say how stale it is and why: age=%v bound=%v doors=%d err=%q", age, bound, doors, lastErr)
	}
}
