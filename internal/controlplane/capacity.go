package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Serving capacity, per model.
//
// The question the gateway asks before it hands a request to the router is
// "can anything take a request for THIS model right now?" — not "is anything
// on this leader serving?". With one model per leader the two are the same;
// on a leader that serves several, a model whose only worker is drained used
// to pass the global check, reach the router, fall to the local engine and
// answer 502 or 404 — an error a client cannot act on — instead of the 503 +
// Retry-After every other "not right now" gets.

// wakingMessage is the answer when nothing holds the model at all on a leader
// that serves only through workers: scale-up from zero, or workers on their way.
const wakingMessage = "no workers are awake for this model — waking (scale-up in progress or floor is 0); retry shortly"

// modelHolders is who holds a model on the workers, by whether they can take
// a request for it (the node rule is store.Node.TakesNewWork).
type modelHolders struct {
	serving  int // node takes new work, placement routable
	draining int // an operator drained the node, or the leader is draining the placement
	lost     int // heartbeats stopped
	asleep   int // resident, engine asleep: wakes on demand
	other    int // loading, failed, a state this version does not serve from
}

func (h modelHolders) total() int { return h.serving + h.draining + h.lost + h.asleep + h.other }

// holdersOf counts the workers holding model. The leader's own "local" row is
// not a worker: the local engine is judged by its health (unavailable).
func (s *Server) holdersOf(ctx context.Context, model string) modelHolders {
	var h modelHolders
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return h
	}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for _, n := range nodes {
		if n.ID == "local" {
			continue
		}
		ps, err := s.store.Placements().GetByNode(ctx, n.ID)
		if err != nil {
			continue
		}
		for _, p := range ps {
			if p.ModelID != model {
				continue
			}
			switch {
			case n.Draining():
				h.draining++
			case !n.Alive(maxAge, now):
				h.lost++
			case !n.TakesNewWork(maxAge, now):
				h.other++
			case p.Status == "" || p.Status == "ready":
				h.serving++
			case p.Status == PlacementSleeping:
				h.asleep++
			case p.Status == "draining":
				h.draining++
			default:
				h.other++
			}
		}
	}
	return h
}

// unavailable says why nothing can take a new request for model right now,
// or "" when something can — or when only the router can tell (a healthy
// local engine may load a model on demand). The caller answers a non-empty
// reason with 503 + Retry-After.
func (s *Server) unavailable(ctx context.Context, model string) string {
	if s.modelServable(ctx, model) {
		return ""
	}
	// Workers hold it and none can take a request: say which it is.
	if h := s.holdersOf(ctx, model); h.total() > 0 {
		return h.reason(model)
	}
	if gang := s.gangReason(ctx, model); gang != "" {
		return gang
	}
	// Nobody holds it. A leader with a working engine of its own lets the
	// router try (the engine answers, or loads on demand). A leader that
	// serves only through workers, with nothing serving at all, is waking.
	if !s.cfg.Router.PullDefaultModel && !s.hasServingCapacity(ctx) {
		return wakingMessage
	}
	return ""
}

// modelServable: a worker that takes requests holds it, its gang is up, or
// the leader's own healthy engine holds it.
func (s *Server) modelServable(ctx context.Context, model string) bool {
	// The request path's common case, kept to the rows the router itself
	// reads: the model's routable placements, and the first holder that
	// takes new work settles it.
	if ready, err := s.store.Placements().GetByModel(ctx, model); err == nil {
		maxAge, now := s.heartbeatMaxAge(), time.Now()
		for _, p := range ready {
			if p.NodeID == "local" {
				continue // the leader's own engine: judged by its health, below
			}
			if n, err := s.store.Nodes().Get(ctx, p.NodeID); err == nil && n != nil && n.TakesNewWork(maxAge, now) {
				return true
			}
		}
	}
	if shards, err := s.store.Shards().GetByModel(ctx, model); err == nil && len(shards) > 0 {
		return servableShardModels(shards, s.routableNodes(ctx))[model]
	}
	if s.engine.Health(ctx) != nil {
		return false
	}
	ps, err := s.store.Placements().GetByNode(ctx, "local")
	if err != nil {
		return false
	}
	for _, p := range ps {
		if p.ModelID == model && (p.Status == "" || p.Status == "ready") {
			return true
		}
	}
	return false
}

// gangReason: the model is sharded and its gang cannot serve.
func (s *Server) gangReason(ctx context.Context, model string) string {
	shards, err := s.store.Shards().GetByModel(ctx, model)
	if err != nil || len(shards) == 0 {
		return ""
	}
	nodes, err := s.store.Nodes().List(ctx)
	if err != nil {
		return ""
	}
	state := map[string]string{}
	maxAge, now := s.heartbeatMaxAge(), time.Now()
	for _, n := range nodes {
		state[n.ID] = n.LiveState(maxAge, now)
	}
	var h modelHolders
	for _, sh := range shards {
		switch state[sh.NodeID] {
		case "draining":
			h.draining++
		case NodeStateLost, "":
			if sh.NodeID != "" && sh.NodeID != "local" {
				h.lost++
			}
		}
	}
	if h.draining+h.lost == 0 {
		return fmt.Sprintf("the sharded model %s is not ready to serve yet; retry shortly", model)
	}
	return fmt.Sprintf("the sharded model %s cannot take new requests: a gang serves as one unit and %s; retry shortly",
		model, strings.Join(h.causes("part"), ", "))
}

// reason is the 503 text for a model that workers hold and none can serve.
func (h modelHolders) reason(model string) string {
	if h.asleep > 0 && h.draining+h.lost+h.other == 0 {
		return wakingMessage
	}
	return fmt.Sprintf("no worker can take a new request for %s right now: %s; retry shortly",
		model, strings.Join(h.causes("worker"), ", "))
}

// causes names each cause with its count, and what ends it.
func (h modelHolders) causes(unit string) []string {
	var out []string
	if h.draining > 0 {
		out = append(out, fmt.Sprintf("%d %s(s) draining — taken out of rotation, new requests resume on undrain or when another worker loads the model", h.draining, unit))
	}
	if h.lost > 0 {
		out = append(out, fmt.Sprintf("%d %s(s) stopped heartbeating — waiting for it to return or be replaced", h.lost, unit))
	}
	if h.asleep > 0 {
		out = append(out, fmt.Sprintf("%d %s(s) asleep — waking", h.asleep, unit))
	}
	if h.other > 0 {
		out = append(out, fmt.Sprintf("%d %s(s) not ready (loading or failed)", h.other, unit))
	}
	return out
}
