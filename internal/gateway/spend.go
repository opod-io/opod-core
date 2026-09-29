package gateway

// The enforcement half (ADR-063): a door polls the leader's spend snapshot and
// enforces from it.
//
// Two decisions Hadi made, and the reasons they are not the obvious ones:
//
//   - a per-key daily quota may lag up to 10 s across doors, AND THE BOUND IS
//     PUBLISHED. Every door subtracting from a shared total in real time would
//     need a round trip per request, which puts the leader back on the request
//     path (ADR-001 says it never is). So the quota is eventually consistent by
//     design, the window is stated, and a customer reads it from the API.
//   - a key's rate limit is a 1/N SHARE per door, rebalanced every 10 s, not a
//     flat division fixed at start. A keep-alive client is pinned to one door
//     for the life of its connection: give every door a fixed 1/N and that
//     client gets 1/N of the rate it was sold, while the other doors sit idle.
//     The share follows the doors the LEADER has heard from, so doors that die
//     hand their share back.
//
// Fail-static: a door that cannot reach the leader keeps enforcing the last
// snapshot it has. It does not fall open (that would sell an unlimited key) and
// it does not fall closed (that would take the endpoint down when the brain
// blinks) — it keeps the last thing the brain said, and says how old it is.

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

// Spend polls the snapshot and answers the two questions the request path asks.
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

// Age is how old the enforced snapshot is, and the bound the leader published.
// A door serving on a snapshot older than the bound is still serving — it just
// cannot claim the quota is accurate, and this is what says so.
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
