package controlplane

// Gateway replicas, leader side (T11.1 slice 1, ADR-063).
//
// One endpoint, one leader, several doors. A gateway is a copied FRONT: it
// accepts the request, checks the key, rate-limits, picks a worker, streams and
// records usage. The leader stays the single BRAIN — the worker registry, the
// join tokens, the plan revision, and the store every usage row lands in.
//
// Two asynchronous halves, both fail-static, so a gateway keeps serving when
// the leader is unreachable and the leader keeps serving when a gateway dies:
//
//   - usage is PUSHED here (POST /admin/v1/usage/push). At-least-once, and
//     deduplicated by a row id the GATEWAY mints, because a retry after a
//     half-written response must not bill twice.
//   - spend is POLLED back (GET /admin/v1/spend). A per-key daily quota may
//     therefore lag, and ADR-063 fixes that bound at 10 s AND publishes it
//     here rather than leaving a customer to discover it.
//
// The rate limit is the part that is easy to get wrong: a flat share hands a
// keep-alive client — pinned to one door — 1/N of the rate it was sold. So the
// snapshot carries each key's limit AND the share this door may use, computed
// from the doors the leader has actually heard from, and the gateways rebalance
// off it every 10 s.

import (
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/opod-io/opod/internal/store"
)

// gatewayLiveFor is how long a gateway counts as a door after its last push.
// It is deliberately longer than the 10 s rebalance: a door that missed one
// push must not make every other door widen its share and overshoot the rate
// the key was sold.
const gatewayLiveFor = 45 * time.Second

// spendLagBoundMS is the bound ADR-063 fixes and publishes: how far a per-key
// quota may lag across doors. It is in the snapshot so a client reads it from
// the product rather than from our documentation.
const spendLagBoundMS = 10_000

// dedupWindow is how many recent gateway row ids the leader remembers. The
// dedup is in memory on purpose: it protects against a gateway's retry, which
// happens within seconds, not against a leader restart — and a restart that
// re-accepted a row is visible as a duplicate in the usage export rather than
// as a silent double charge. Bounded so a busy endpoint cannot grow it without
// limit.
const dedupWindow = 20_000

type gatewayFront struct {
	mu       sync.Mutex
	lastSeen map[string]time.Time // gateway id → last push
	load     map[string]doorLoad  // gateway id → its own live load, from the same push
	seen     map[string]struct{}  // row ids already recorded
	order    []string             // insertion order, for the bounded window
}

// doorLoad is one door's own load as it last reported it. The leader keeps
// these because the ENDPOINT's total is the sum and no single door has it —
// a scaler for the doors would otherwise be reading one door's share of the
// traffic and calling it the load.
type doorLoad struct {
	inFlight int64
	rpm1m    int64
	at       time.Time
}

func newGatewayFront() *gatewayFront {
	return &gatewayFront{lastSeen: map[string]time.Time{}, load: map[string]doorLoad{}, seen: map[string]struct{}{}}
}

// beat records that this door is alive and what it is carrying. It is called
// once per push, INCLUDING a push with no rows: a door serving nothing is still
// a door, and one that dropped out of the live set would make every other door
// widen its share of a key's rate.
func (g *gatewayFront) beat(gw string, inFlight, rpm1m int64, now time.Time) {
	if gw == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lastSeen[gw] = now
	g.load[gw] = doorLoad{inFlight: inFlight, rpm1m: rpm1m, at: now}
}

// doorTotals sums what the live doors are carrying, and says how many reported.
// A door heard from but with no sample counts as a door and not as a reporter,
// the same distinction /loadz makes for workers.
func (g *gatewayFront) doorTotals(now time.Time) (inFlight, rpm1m int64, reporting int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for gw, at := range g.lastSeen {
		if now.Sub(at) > gatewayLiveFor {
			continue
		}
		ld, ok := g.load[gw]
		if !ok || now.Sub(ld.at) > gatewayLiveFor {
			continue
		}
		inFlight += ld.inFlight
		rpm1m += ld.rpm1m
		reporting++
	}
	return inFlight, rpm1m, reporting
}

// note records a door and returns whether this row id is new.
func (g *gatewayFront) note(gw, rowID string, now time.Time) (fresh bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if gw != "" {
		g.lastSeen[gw] = now
	}
	if rowID == "" {
		return true // no id: the caller takes the at-least-once risk it chose
	}
	if _, dup := g.seen[rowID]; dup {
		return false
	}
	g.seen[rowID] = struct{}{}
	g.order = append(g.order, rowID)
	if len(g.order) > dedupWindow {
		drop := len(g.order) - dedupWindow
		for _, id := range g.order[:drop] {
			delete(g.seen, id)
		}
		g.order = g.order[drop:]
	}
	return true
}

// doors is the gateways heard from within gatewayLiveFor, and is never less
// than 1: the leader's own front door serves when no gateway exists, and a
// share of 1/0 has no meaning.
func (g *gatewayFront) doors(now time.Time) (n int, names []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for gw, at := range g.lastSeen {
		if now.Sub(at) <= gatewayLiveFor {
			names = append(names, gw)
		}
	}
	if len(names) == 0 {
		return 1, nil
	}
	return len(names), names
}

// seenDoors is every door the leader has EVER heard from with the age of its
// last push, live or not. doors() deliberately hides a door that went quiet
// (a share of 1/N must not count a dead door); this is where it is still
// visible, because "a door stopped pushing" is a fact an operator needs and
// no other surface carries it.
func (g *gatewayFront) seenDoors(now time.Time) []gatewayDoor {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]gatewayDoor, 0, len(g.lastSeen))
	for gw, at := range g.lastSeen {
		out = append(out, gatewayDoor{
			Gateway:    gw,
			LastPushS:  int(now.Sub(at).Seconds()),
			Live:       now.Sub(at) <= gatewayLiveFor,
			LastPushTS: at.Unix(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Gateway < out[j].Gateway })
	return out
}

// gatewayDoor is one front door as the leader sees it.
type gatewayDoor struct {
	Gateway    string `json:"gateway"`
	Live       bool   `json:"live"`
	LastPushS  int    `json:"last_push_s"`
	LastPushTS int64  `json:"last_push_ts"`
}

// listGateways answers GET /admin/v1/gateways.
func (s *Server) listGateways(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	live, _ := s.gateways.doors(now)
	writeJSON(w, http.StatusOK, map[string]any{
		"doors":          live,
		"live_for_s":     int(gatewayLiveFor.Seconds()),
		"lag_bound_s":    spendLagBoundMS / 1000,
		"gateways":       s.gateways.seenDoors(now),
		"dedup_window":   dedupWindow,
		"leader_is_door": true,
	})
}

// pushUsageRequest is what a gateway sends. Each row carries the id the gateway
// minted for it; the leader's own row id stays the store's.
type pushUsageRequest struct {
	Gateway string `json:"gateway"`
	Rows    []struct {
		ID               string    `json:"id"`
		TS               time.Time `json:"ts"`
		APIKeyID         string    `json:"api_key_id"`
		UserID           string    `json:"user_id"`
		Model            string    `json:"model"`
		Protocol         string    `json:"protocol"`
		PromptTokens     int       `json:"prompt_tokens"`
		CompletionTokens int       `json:"completion_tokens"`
		LatencyMS        int       `json:"latency_ms"`
		TTFTMS           int       `json:"ttft_ms"`
		Outcome          string    `json:"outcome"`
		NodeID           string    `json:"node_id"`
	} `json:"rows"`
	// InFlight and RPM1m are the DOOR's own live load at the moment of the
	// push. The leader publishes the sum (GET /gatewayz) because that total is
	// what a scaler for the doors must read: any single door sees only its
	// share, and keep-alive makes that share uneven.
	InFlight int64 `json:"in_flight"`
	RPM1m    int64 `json:"rpm_1m"`
}

// pushUsage accepts usage rows a gateway recorded. Admin-keyed like every other
// /admin/v1 route.
func (s *Server) pushUsage(w http.ResponseWriter, r *http.Request) {
	var req pushUsageRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "usage push: "+err.Error())
		return
	}
	now := time.Now()
	// The door first, the rows second: an empty push is a heartbeat and must
	// still count, and a door whose rows are all duplicates is no less alive.
	s.gateways.beat(req.Gateway, req.InFlight, req.RPM1m, now)
	accepted, duplicate := 0, 0
	for _, row := range req.Rows {
		if !s.gateways.note(req.Gateway, row.ID, now) {
			duplicate++
			continue
		}
		ts := row.TS
		if ts.IsZero() {
			ts = now
		}
		u := store.Usage{
			TS: ts, APIKeyID: row.APIKeyID, UserID: row.UserID, Model: row.Model, Protocol: row.Protocol,
			PromptTokens: row.PromptTokens, CompletionTokens: row.CompletionTokens,
			LatencyMS: row.LatencyMS, TTFTMS: row.TTFTMS, Outcome: row.Outcome, NodeID: row.NodeID,
		}
		if err := s.store.Usage().Record(r.Context(), u); err != nil {
			// Partial success is reported, not hidden: the gateway retries the
			// rest, and the rows already written are deduplicated on the retry.
			writeJSON(w, http.StatusInternalServerError, map[string]any{
				"accepted": accepted, "duplicate": duplicate, "error": err.Error(),
			})
			return
		}
		accepted++
	}
	writeJSON(w, http.StatusOK, map[string]any{"accepted": accepted, "duplicate": duplicate})
}

// spendKey is one key's ceiling and the share of it this door may use.
type spendKey struct {
	APIKeyID string `json:"api_key_id"`
	// TokensToday is what the leader has recorded since the quota window
	// opened — the number a gateway subtracts from the daily quota.
	TokensToday int64 `json:"tokens_today"`
	QuotaDaily  int64 `json:"quota_daily_tokens,omitempty"`
	RPMLimit    int   `json:"rpm_limit,omitempty"`
	TPMLimit    int   `json:"tpm_limit,omitempty"`
	// RPMShare / TPMShare are this door's slice of the ceiling: the limit
	// divided by the doors the leader has heard from, never below 1 for a
	// limited key (a share that rounded to 0 would refuse every request).
	RPMShare int `json:"rpm_share,omitempty"`
	TPMShare int `json:"tpm_share,omitempty"`
}

// spendSnapshot is what a gateway polls.
type spendSnapshot struct {
	TS int64 `json:"ts"`
	// Doors is how many fronts the leader believes are serving, and Gateways
	// names them. A gateway that finds itself missing knows its pushes are not
	// arriving, which is the only symptom it would otherwise have.
	Doors    int      `json:"doors"`
	Gateways []string `json:"gateways,omitempty"`
	// LagBoundMS is published, not implied (ADR-063).
	LagBoundMS   int        `json:"lag_bound_ms"`
	WindowOpened int64      `json:"window_opened_unix"`
	Keys         []spendKey `json:"keys"`
}

// spend serves the snapshot every gateway polls: what has been spent per key,
// the ceilings, and this door's share of them.
func (s *Server) spend(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	doors, names := s.gateways.doors(now)
	// The quota window is the day the daily quota is counted over — the same
	// window the leader's own middleware uses, so a gateway and the leader
	// never disagree about when it opened.
	opened := now.UTC().Truncate(24 * time.Hour)
	out := spendSnapshot{TS: now.Unix(), Doors: doors, Gateways: names, LagBoundMS: spendLagBoundMS, WindowOpened: opened.Unix(), Keys: []spendKey{}}
	keys, err := s.store.APIKeys().List(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}
	share := func(limit int) int {
		if limit <= 0 {
			return 0 // unlimited stays unlimited: a share of "no ceiling" is no ceiling
		}
		if n := limit / doors; n > 0 {
			return n
		}
		return 1 // never 0: a door that may serve nothing is a door that is down
	}
	for _, k := range keys {
		if k.Revoked || k.Scope == "node" {
			continue // a node token is not a customer key and has no spend
		}
		row := spendKey{APIKeyID: k.ID, QuotaDaily: k.QuotaDailyTokens, RPMLimit: k.RPMLimit, TPMLimit: k.TPMLimit,
			RPMShare: share(k.RPMLimit), TPMShare: share(k.TPMLimit)}
		if k.QuotaDailyTokens > 0 {
			// Only a key with a quota costs a query: the rest have nothing to
			// compare a total with.
			if n, err := s.store.Usage().SumTokensSince(r.Context(), k.ID, opened); err == nil {
				row.TokensToday = n
			}
		}
		out.Keys = append(out.Keys, row)
	}
	writeJSON(w, http.StatusOK, out)
}
