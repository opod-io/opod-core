// Package gateway is the front-door half of a leader (T11.1, ADR-063): the
// pieces a copied FRONT needs that a leader does not.
//
// One endpoint, one leader, several doors. A gateway accepts the request,
// checks the key, rate-limits, picks a worker, streams and records usage; the
// leader stays the single brain that owns the worker registry, the join tokens
// and the plan revision. So a gateway needs two things a leader gets for free:
// a worker list it did not learn from heartbeats, and somewhere to put usage
// rows it must not be the only copy of.
//
// Everything here is FAIL-STATIC. A gateway whose leader is unreachable keeps
// serving the workers it already knows and keeps queueing its usage; it never
// stops serving because the brain is down, which is the whole point of
// splitting them (ADR-001: the control plane is never on the request path, and
// neither is the leader once a door has a list).
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// RegistryTTL is how long a mirrored worker list is trusted for routing. Past
// it the list is STALE: still used, because serving beats refusing, but no
// worker learned from it is added.
const RegistryTTL = 30 * time.Second

// leaderNode is one row of the leader's GET /admin/v1/nodes. The leader embeds
// its stored row (PascalCase, no JSON tags) and decorates it with the derived
// state, so both keys are on the wire; the derived one is what routing needs,
// and only ONE of them can be decoded — encoding/json folds names
// case-insensitively and drops both fields when two tags differ only by case.
type leaderNode struct {
	ID       string `json:"ID"`
	Hostname string `json:"Hostname"`
	Address  string `json:"Address"`
	// State is the LIVE state the leader derives: ready | draining | lost |
	// engine-silent | removed. A door must not route to a node the brain has
	// already taken out of rotation.
	State               string `json:"state"`
	HeartbeatAgeSeconds int64  `json:"heartbeat_age_seconds"`
	// Placements is what the worker holds resident. A door routes BY placement:
	// mirroring the machines without them gives a door a list it cannot send a
	// single request to (every pick would find no holder and answer 503).
	Placements []store.Placement `json:"placements"`
	// WorkerToken is the per-node credential the worker minted at join, which
	// the leader keeps on the row precisely so it can CALL THAT WORKER BACK.
	// A door dispatches to the same workers, so it needs the same credential:
	// without it the worker answers every dispatched request `401
	// Unauthorized` and the door turns it into a 502 (measured on the
	// design-partner cell, 2026-09-21 — the first request a door ever
	// dispatched).
	//
	// It is a secret on the wire, and it travels the way every other one does
	// here: over the leader's admin surface, which is admin-keyed and
	// in-cluster, to a process the control plane gave that admin token to. A
	// door holds nothing it cannot rebuild from the leader (ADR-063), and this
	// is rebuilt on every sync like the rest of the row.
	WorkerToken string `json:"WorkerToken"`
}

// Mirror keeps a gateway's local (in-memory) store looking like the leader's
// worker registry.
type Mirror struct {
	leaderURL string
	token     string
	http      *http.Client
	st        store.Store
	now       func() time.Time

	mu      sync.Mutex
	lastOK  time.Time
	lastErr string
	known   map[string]bool // node ids this door has ever been told about
}

// NewMirror builds the mirror. st is the gateway's own store — in managed mode
// an in-memory, rebuildable cache, which is exactly the right durability for a
// copy of somebody else's registry.
func NewMirror(leaderURL, token string, st store.Store) *Mirror {
	return &Mirror{leaderURL: leaderURL, token: token, st: st,
		http: &http.Client{Timeout: 10 * time.Second}, now: time.Now, known: map[string]bool{}}
}

// Fresh reports whether the mirrored list is inside RegistryTTL, and how old it
// is. A door that has never reached the leader is not fresh and has no age.
func (m *Mirror) Fresh() (fresh bool, age time.Duration, lastErr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastOK.IsZero() {
		return false, 0, m.lastErr
	}
	age = m.now().Sub(m.lastOK)
	return age <= RegistryTTL, age, m.lastErr
}

// Sync pulls the leader's registry once and writes it into the local store.
//
// The rule this function exists for, and the answer to the row's open question:
// **a STALE list may keep workers, never add one.** A door that cannot reach
// the brain goes on routing to workers it already knows and that still answer;
// it does not adopt a worker it learned from a list it cannot refresh, because
// that worker may have been drained, moved or destroyed since — and a door that
// adds one is a door sending requests somewhere the brain has stopped
// accounting for.
func (m *Mirror) Sync(ctx context.Context) error {
	rows, err := m.fetch(ctx)
	if err != nil {
		m.mu.Lock()
		m.lastErr = err.Error()
		m.mu.Unlock()
		return err
	}
	fresh, _, _ := m.Fresh()
	m.mu.Lock()
	first := m.lastOK.IsZero()
	m.mu.Unlock()
	// The very first sync is always allowed to populate: a door with no list at
	// all cannot serve, and "stale" is about a list that went cold, not about
	// not having one yet.
	adopt := fresh || first

	for _, n := range rows {
		m.mu.Lock()
		newToThisDoor := !m.known[n.ID]
		m.mu.Unlock()
		if newToThisDoor && !adopt {
			continue // stale list: keep what we have, add nothing
		}
		// The leader's DERIVED state is what routing must obey: a node it calls
		// lost or engine-silent is out, whatever its stored row says.
		row := store.Node{ID: n.ID, Hostname: n.Hostname, Address: n.Address, State: n.State,
			WorkerToken:   n.WorkerToken,
			LastHeartbeat: m.now().Add(-time.Duration(n.HeartbeatAgeSeconds) * time.Second)}
		if err := m.st.Nodes().Upsert(ctx, row); err != nil {
			return fmt.Errorf("mirror node %s: %w", n.ID, err)
		}
		// ReplaceForNode, not Upsert: a model the leader no longer lists on this
		// worker must LEAVE the door's view, or the door goes on routing to a
		// worker that unloaded it and every one of those requests fails.
		if err := m.st.Placements().ReplaceForNode(ctx, n.ID, n.Placements); err != nil {
			return fmt.Errorf("mirror placements of %s: %w", n.ID, err)
		}
		m.mu.Lock()
		m.known[n.ID] = true
		m.mu.Unlock()
	}
	// Gangs: a SHARDED endpoint has no placement rows at all — its capacity is
	// a coordinator plus its parts — so a door with no shard rows cannot route
	// to a gang however fresh its node list is.
	if adopt {
		if err := m.syncShards(ctx); err != nil {
			m.mu.Lock()
			m.lastErr = err.Error()
			m.mu.Unlock()
			return err
		}
	}
	m.mu.Lock()
	m.lastOK, m.lastErr = m.now(), ""
	m.mu.Unlock()
	return nil
}

// syncShards makes the door's shard rows the leader's, exactly: parts the
// leader no longer lists are removed, because a coordinator address that moved
// is worse than none — the door would stream to a port nothing answers on.
func (m *Mirror) syncShards(ctx context.Context) error {
	rows, err := m.fetchShards(ctx)
	if err != nil {
		return err
	}
	local, err := m.st.Shards().List(ctx)
	if err != nil {
		return fmt.Errorf("mirror shards: local list: %w", err)
	}
	want := make(map[string]bool, len(rows))
	for _, sh := range rows {
		want[sh.ID] = true
	}
	for _, sh := range local {
		if !want[sh.ID] {
			if err := m.st.Shards().Delete(ctx, sh.ID); err != nil {
				return fmt.Errorf("mirror shards: drop %s: %w", sh.ID, err)
			}
		}
	}
	// Replace rather than patch: Create is an INSERT and a part's address,
	// status and process id all move over its life. Delete-then-create keeps
	// the row equal to the leader's without a second write path.
	for _, sh := range rows {
		if err := m.st.Shards().Delete(ctx, sh.ID); err != nil {
			return fmt.Errorf("mirror shards: replace %s: %w", sh.ID, err)
		}
		if err := m.st.Shards().Create(ctx, sh); err != nil {
			return fmt.Errorf("mirror shards: %s: %w", sh.ID, err)
		}
	}
	return nil
}

func (m *Mirror) fetch(ctx context.Context) ([]leaderNode, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.leaderURL+"/admin/v1/nodes", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("leader %s answered %s", m.leaderURL, resp.Status)
	}
	var out []leaderNode
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// fetchShards reads the leader's gang parts.
func (m *Mirror) fetchShards(ctx context.Context) ([]store.Shard, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.leaderURL+"/admin/v1/shards", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("leader %s answered %s for its shards", m.leaderURL, resp.Status)
	}
	var out []store.Shard
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// Run syncs every interval until ctx is done. A failure is never fatal: the
// door keeps serving what it knows and Fresh() tells the truth about the age.
func (m *Mirror) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	_ = m.Sync(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = m.Sync(ctx)
		}
	}
}
