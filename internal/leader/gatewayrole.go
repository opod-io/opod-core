package leader

// The gateway role, wired (T11.1 slice 2, ADR-063).
//
// A door is the same process as a leader with three differences, and each one
// is a consequence of there being exactly one brain:
//
//   - it serves /v1 and the probe listener, and NOT /admin/v1. The worker
//     registry, the join tokens, the shard calls and the plan revision belong
//     to one process; a door that exposed them would be a second place a
//     worker could join or a gang be torn down.
//   - its worker list is MIRRORED from the leader rather than learned from
//     heartbeats, and a stale list keeps workers but adopts none.
//   - its usage rows are PUSHED to the leader (the single writer the store was
//     built for) and its ceilings come from the spend snapshot it polls back.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/opod-io/opod/internal/gateway"
	"github.com/opod-io/opod/internal/store"
)

// gatewayLoops are the three background loops a door runs.
type gatewayLoops struct {
	mirror *gateway.Mirror
	push   *gateway.Pusher
	spend  *gateway.Spend
	id     string
}

// mirrorEvery is how often a door refreshes its worker list. Comfortably
// inside gateway.RegistryTTL, so one missed poll does not make the list stale.
const mirrorEvery = 10 * time.Second

// pushEvery batches usage rather than sending a request per request: a door
// that pushed synchronously would put the leader back on the request path,
// which is the one thing ADR-001 forbids.
const pushEvery = 5 * time.Second

// StartGatewayRole prepares the door's loops. It refuses rather than starting
// half a gateway: a door with no leader URL cannot mirror a registry, and one
// that served /v1 with an empty worker list would answer every request with a
// 503 that looks like an outage instead of a misconfiguration.
func (s *Server) StartGatewayRole(ctx context.Context) error {
	if !s.isGateway() {
		return nil
	}
	leaderURL := strings.TrimRight(strings.TrimSpace(s.cfg.Env.LeaderURL), "/")
	if leaderURL == "" {
		return fmt.Errorf("OPOD_ROLE=gateway needs OPOD_LEADER_URL (http://leader:8080): a door mirrors the leader's registry and pushes its usage there")
	}
	token := strings.TrimSpace(s.cfg.Auth.AdminToken)
	if token == "" {
		return fmt.Errorf("OPOD_ROLE=gateway needs an admin token (auth.admin_token / OPOD_ADMIN_TOKEN): the registry and spend reads are admin-keyed like every other /admin/v1 call")
	}
	id := strings.TrimSpace(s.cfg.Env.GatewayID)
	if id == "" {
		if h, err := os.Hostname(); err == nil {
			id = h
		} else {
			id = "gateway"
		}
	}
	// A leader with a minted certificate is trusted through the same variable a
	// worker uses. A CA that does not load is a refusal, not a fallback to the
	// system roots: that fallback is a door that cannot reach its leader and
	// does not say why.
	trust, err := gateway.LeaderTransport(strings.TrimSpace(s.cfg.Env.LeaderCA))
	if err != nil {
		return fmt.Errorf("OPOD_ROLE=gateway: %w", err)
	}
	s.front = &gatewayLoops{
		mirror: gateway.NewMirror(leaderURL, token, s.store),
		push:   gateway.NewPusher(leaderURL, token, id),
		spend:  gateway.NewSpend(leaderURL, token),
		id:     id,
	}
	s.front.mirror.Trust(trust)
	// The workers' slot counts ride the mirrored registry; this door counts
	// its own dispatches against them (ADR-091, /gatewayz slot_scope).
	s.front.mirror.OnSlots = func(id string, n int) {
		if old, _ := s.slotsOfKey(id); old == n {
			return
		}
		s.noteSlots(id, n)
		s.capacityChanged()
	}
	s.front.push.Trust(trust)
	s.front.spend.Trust(trust)
	// A door never receives a heartbeat, so the heartbeat-AGE rule is not a
	// liveness rule for it — it is a clock on how long ago the LEADER last saw
	// the worker, copied with the row. While the leader restarts (every rollout,
	// every failover) the mirror cannot refresh, those copied timestamps age
	// past the bound, and the door starts answering 503 for workers that are
	// serving: measured on the design-partner cell, 2026-09-21 — a door kept
	// serving for 18 requests after the leader was deleted and then refused 25.
	//
	// The authority on a worker's liveness is the brain, and it already travels
	// with the row: the mirror copies the leader's DERIVED state (ready |
	// draining | lost | engine-silent) and refuses to route to anything the
	// leader has taken out. So a door obeys that and turns its own age rule off.
	// What remains is fail-static by design (ADR-063): while the brain is
	// unreachable a door goes on routing to the workers it knows, and a worker
	// that has really gone fails the dispatch, which the router's next-worker
	// walk and the 503 + Retry-After already handle. How stale the list is is
	// published rather than hidden — /gatewayz carries registry_age_s.
	s.router.SetHeartbeatMaxAge(0)

	// Every push carries this door's own load, so the leader can publish the
	// endpoint's total (GET /gatewayz on the leader) and a scaler for the doors
	// reads the traffic instead of one door's uneven share of it.
	s.front.push.SetLoad(func() (int64, int64) {
		return atomic.LoadInt64(&s.load.inFlight), s.load.sum(&s.load.reqRing, &s.load.reqSec, time.Now())
	})
	// The first mirror is synchronous: a door that starts serving before it has
	// a worker list answers 503 for its first seconds, and a rollout would read
	// that as a door that never came up.
	first, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.front.mirror.Sync(first); err != nil {
		return fmt.Errorf("gateway %s could not read the leader's registry at %s: %w", id, leaderURL, err)
	}
	if err := s.front.spend.Poll(first); err != nil {
		// Not fatal: enforcement is fail-static and the poller retries. The
		// registry is what a door cannot serve without.
		s.log.Warn("gateway could not read the spend snapshot yet — enforcing nothing until it can", "leader", leaderURL, "err", err)
	}
	go s.front.mirror.Run(ctx, mirrorEvery)
	go s.front.push.Run(ctx, pushEvery, 500)
	go s.front.spend.Run(ctx)
	s.log.Info("gateway role", "id", id, "leader", leaderURL, "serves", "/v1 + probes", "admin_surface", "none")
	return nil
}

// recordUsageForGateway queues a usage row for the push instead of treating the
// local store as the record. Called by the request path when this process is a
// door; the local row stays for the door's own /loadz and is not the copy that
// counts.
func (s *Server) recordUsageForGateway(u store.Usage, rowID string) {
	if s.front == nil {
		return
	}
	// The id is namespaced by the door: two gateways can mint the same request
	// id, and the leader dedups by this string alone.
	s.front.push.Add(gateway.RowFrom(s.front.id+":"+rowID, u))
	// The quota this door enforces is the leader's number plus what it has
	// served since, so tell the enforcer immediately rather than waiting for
	// the row to come back in a snapshot.
}

// gatewayz answers GET /gatewayz in whichever role this process is.
func (s *Server) gatewayz(w http.ResponseWriter, _ *http.Request) {
	if st := s.GatewayStatus(); st != nil {
		writeJSON(w, http.StatusOK, st)
		return
	}
	now := time.Now()
	inFlight, rpm, reporting := s.gateways.doorTotals(now)
	live, _ := s.gateways.doors(now)
	// The leader is a door too: it serves /v1 itself, and a total that left its
	// own traffic out would under-read the endpoint by exactly one door's worth.
	inFlight += atomic.LoadInt64(&s.load.inFlight)
	rpm += s.load.sum(&s.load.reqRing, &s.load.reqSec, now)
	writeJSON(w, http.StatusOK, map[string]any{
		"role": "leader",
		// doors counts the leader's own front, so it is never 0 — and it is the
		// same number the spend snapshot divides a key's rate by.
		"doors":           live,
		"doors_reporting": reporting + 1,
		// The per-door averages are what an autoscaler compares against a
		// target; the totals are for a human reading the page.
		"doors_in_flight":    inFlight,
		"doors_rpm_1m":       rpm,
		"in_flight_per_door": ratePerDoor(inFlight, live),
		"rpm_1m_per_door":    ratePerDoor(rpm, live),
		"live_for_s":         int(gatewayLiveFor.Seconds()),
		"spend_lag_bound_s":  spendLagBoundMS / 1000,
		"gateways":           s.gateways.seenDoors(now),
		"admission":          s.admissionFairness(live),
	})
}

// admissionFairness states what request classes and worker slots promise
// across doors (ADR-086 §6, ADR-091), beside the spend bound and for the same
// reason: each door — the leader's own front included — ranks only the
// requests it holds and counts only the requests it dispatched. Inside one
// door class order is strict, two flows of one class are served within one
// request of each other, and no worker gets more requests than its slots.
// Across N doors a flow that reaches every door can be served up to N times as
// often as a flow of its class that reaches one, a lower class held at one
// door can be served while a higher class waits at another, and a worker can
// be sent up to N times its slots at once (the excess waits in its engine's
// queue). With one door every bound is exact. Doors do not coordinate.
func (s *Server) admissionFairness(doors int) map[string]any {
	if doors < 1 {
		doors = 1
	}
	out := map[string]any{
		"classes":                     s.admission.config().classed,
		"scope":                       "per_door",
		"flow_share_skew_max":         doors,
		"class_order_across_doors":    doors == 1,
		"slot_scope":                  "per_door",
		"worker_slots_overcommit_max": doors,
	}
	for k, v := range s.admissionGovernance() {
		out[k] = v
	}
	return out
}

// ratePerDoor is a total divided by the doors carrying it, rounded UP: a
// scaler comparing a per-door average against a target must not be told 0.9
// when a door is carrying one request.
func ratePerDoor(total int64, doors int) int64 {
	if doors <= 0 {
		return total
	}
	return (total + int64(doors) - 1) / int64(doors)
}

// GatewayStatus is what a door reports about itself: the staleness a console
// must show (the row's own requirement) and the backlog, which is the only
// symptom a queue that is not draining otherwise has.
func (s *Server) GatewayStatus() map[string]any {
	if s.front == nil {
		return nil
	}
	fresh, age, mirrorErr := s.front.mirror.Fresh()
	queued, sent, dropped, pushErr := s.front.push.Stats()
	spendAge, bound, doors, spendErr := s.front.spend.Age()
	out := map[string]any{
		"gateway":           s.front.id,
		"registry_fresh":    fresh,
		"registry_age_s":    int(age.Seconds()),
		"usage_queued":      queued,
		"usage_sent":        sent,
		"usage_dropped":     dropped,
		"spend_age_s":       int(spendAge.Seconds()),
		"spend_lag_bound_s": int(bound.Seconds()),
		"doors":             doors,
		"admission":         s.admissionFairness(doors),
	}
	for k, v := range map[string]string{"registry_error": mirrorErr, "usage_error": pushErr, "spend_error": spendErr} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}
