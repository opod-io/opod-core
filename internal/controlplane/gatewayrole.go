package controlplane

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
	"os"
	"strings"
	"time"

	"github.com/opod-io/opod/internal/api"
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
	s.front = &gatewayLoops{
		mirror: gateway.NewMirror(leaderURL, token, s.store),
		push:   gateway.NewPusher(leaderURL, token, id),
		spend:  gateway.NewSpend(leaderURL, token),
		id:     id,
	}
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
	s.front.spend.Spent(u.APIKeyID, int64(u.PromptTokens+u.CompletionTokens))
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
	}
	for k, v := range map[string]string{"registry_error": mirrorErr, "usage_error": pushErr, "spend_error": spendErr} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// spendSource is the door's view of what a key has spent across every door,
// and nil on a leader — where the local store IS the record and asking a
// snapshot would only add lag to a number this process already knows.
//
// Both return the INTERFACE, nil-checked here: handing back a typed nil
// *gateway.Spend would arrive downstream as a non-nil interface and every
// leader would then call Share on a nil receiver.
func (s *Server) spendSource() api.SpendSource {
	if s.front == nil {
		return nil
	}
	return s.front.spend
}

// shareSource is the door's slice of each key's per-minute ceilings; nil on a
// leader, whose ceilings are the whole of what the key bought.
func (s *Server) shareSource() api.ShareSource {
	if s.front == nil {
		return nil
	}
	return s.front.spend
}
