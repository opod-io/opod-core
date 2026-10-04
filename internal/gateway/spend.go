package gateway

// The spend poll (ADR-063): a door polls the leader's spend snapshot every
// SpendPollEvery and reports, on /gatewayz, how many doors the leader counts and
// how old its own copy is.
//
// The snapshot used to carry each key's daily quota and rate limit, which a door
// enforced from it (a 1/N share per door, eventually consistent within the
// published bound). Per-key quotas and rate limits left core on 2026-09-28
// (ADR-077): they belong to the application layer in front of an endpoint. What
// remains is the door count and the lag bound.
//
// Fail-static: a door that cannot reach the leader keeps the last snapshot it
// has, and says how old it is.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// SpendPollEvery is the rebalance period ADR-063 fixed. It is also what makes
// the published lag bound true, so the two move together or neither does.
const SpendPollEvery = 10 * time.Second

// Snapshot is the leader's answer.
type Snapshot struct {
	TS         int64 `json:"ts"`
	Doors      int   `json:"doors"`
	LagBoundMS int   `json:"lag_bound_ms"`
}

// Spend polls the snapshot and keeps the last good one.
type Spend struct {
	leaderURL string
	token     string
	http      *http.Client
	now       func() time.Time

	mu       sync.RWMutex
	snap     Snapshot
	at       time.Time
	lastErr  string
	localUse map[string]int64 // key → tokens this door has spent since the last snapshot
}

func NewSpend(leaderURL, token string) *Spend {
	return &Spend{leaderURL: leaderURL, token: token, now: time.Now,
		http: &http.Client{Timeout: 10 * time.Second}, localUse: map[string]int64{}}
}

// Poll refreshes the snapshot. On success this door's local tally resets: the
// leader's number now includes everything it has pushed and been credited for,
// and keeping the local count on top would double it.
func (s *Spend) Poll(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.leaderURL+"/admin/v1/spend", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := s.http.Do(req)
	if err != nil {
		s.mu.Lock()
		s.lastErr = err.Error()
		s.mu.Unlock()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := fmt.Sprintf("leader answered %s", resp.Status)
		s.mu.Lock()
		s.lastErr = e
		s.mu.Unlock()
		return fmt.Errorf("%s", e)
	}
	var snap Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		return err
	}
	s.mu.Lock()
	s.snap, s.at, s.lastErr = snap, s.now(), ""
	s.localUse = map[string]int64{}
	s.mu.Unlock()
	return nil
}

// Age is how old the held snapshot is, and the bound the leader published.
// A door holding a snapshot older than the bound is still serving; this is what
// says its door count is stale.
func (s *Spend) Age() (age time.Duration, bound time.Duration, doors int, lastErr string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.at.IsZero() {
		return 0, time.Duration(s.snap.LagBoundMS) * time.Millisecond, s.snap.Doors, s.lastErr
	}
	return s.now().Sub(s.at), time.Duration(s.snap.LagBoundMS) * time.Millisecond, s.snap.Doors, s.lastErr
}

// Run polls every SpendPollEvery until ctx is done.
func (s *Spend) Run(ctx context.Context) {
	t := time.NewTicker(SpendPollEvery)
	defer t.Stop()
	_ = s.Poll(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = s.Poll(ctx)
		}
	}
}
