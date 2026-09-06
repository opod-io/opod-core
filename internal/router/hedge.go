package router

import (
	"context"
	"sort"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func (r *Router) chatHedged(ctx context.Context, req engines.ChatRequest, replicas int) (<-chan engines.StreamEvent, error) {
	ctx, span := tracer.Start(ctx, "router.Chat.hedged",
		trace.WithAttributes(
			attribute.String("opod.model.requested", req.Model),
			attribute.Int("opod.hedge.replicas", replicas),
		),
	)
	candidates := r.hedgePickWorkers(ctx, req.Model, replicas)
	if len(candidates) < 2 {
		span.SetAttributes(attribute.Int("opod.hedge.candidates", len(candidates)))
		span.End()
		return nil, nil // fall through
	}
	span.SetAttributes(attribute.Int("opod.hedge.candidates", len(candidates)))

	type result struct {
		idx    int
		stream <-chan engines.StreamEvent
		cancel context.CancelFunc
		nodeID string
		err    error
	}
	resultCh := make(chan result, len(candidates))
	// Per-replica cancels live outside the goroutines so the winner path
	// can cancel the still-running losers immediately instead of waiting
	// for them to come back on resultCh.
	cancels := make([]context.CancelFunc, len(candidates))
	for i, w := range candidates {
		i, w := i, w
		thisCtx, thisCancel := context.WithCancel(ctx)
		cancels[i] = thisCancel
		go func() {
			r.incInflight(w.nodeID, req.Model)
			s, err := w.engine.Chat(thisCtx, req)
			if err != nil {
				thisCancel()
				r.decInflight(w.nodeID, req.Model)
			}
			resultCh <- result{idx: i, stream: s, cancel: thisCancel, nodeID: w.nodeID, err: err}
		}()
	}

	// Wait only until the FIRST success — a hung replica must not stall
	// the request (cutting that tail is the whole point of hedging).
	// Failures are tolerated until every replica has failed.
	var winner result
	var firstErr error
	failed := 0
	for winner.stream == nil && failed < len(candidates) {
		res := <-resultCh
		if res.err != nil {
			failed++
			if firstErr == nil {
				firstErr = res.err
			}
			metrics.ObserveRouterHedge("error")
			continue
		}
		winner = res
		metrics.ObserveRouterHedge("win")
	}
	if winner.stream == nil {
		span.SetStatus(codes.Error, "all hedge replicas failed")
		span.End()
		return nil, firstErr
	}
	// Cancel the losers right away so they stop generating, then reap
	// their results in the background. A loser whose stream opened still
	// needs its inflight counter released and its buffered events
	// drained — a cancelled producer closes its channel, so the drain
	// terminates. Losers that fail decrement their own counter in the
	// per-replica goroutine above.
	for i, cancel := range cancels {
		if i != winner.idx {
			cancel()
		}
	}
	if remaining := len(candidates) - failed - 1; remaining > 0 {
		go func() {
			for i := 0; i < remaining; i++ {
				res := <-resultCh
				if res.err != nil {
					metrics.ObserveRouterHedge("error")
					continue
				}
				r.decInflight(res.nodeID, req.Model)
				metrics.ObserveRouterHedge("cancelled")
				go drainWithTimeout(res.stream, 30*time.Second)
			}
		}()
	}
	span.SetAttributes(attribute.String("opod.hedge.winner", winner.nodeID))
	span.SetStatus(codes.Ok, "hedge winner")
	// Defer winner's cleanup to a goroutine that watches the stream
	// close. (The winning attempt's incInflight stays in effect until
	// the stream drains.)
	out := make(chan engines.StreamEvent, 16)
	go func() {
		defer winner.cancel()
		defer r.decInflight(winner.nodeID, req.Model)
		defer close(out)
		defer span.End()
		for ev := range winner.stream {
			select {
			case out <- ev:
			case <-ctx.Done():
				go drainWithTimeout(winner.stream, 30*time.Second)
				return
			}
		}
	}()
	return out, nil
}

// hedgeCandidate is a small carrier struct so the hedged path doesn't
// have to round-trip nodeIDs through the picker.
type hedgeCandidate struct {
	engine engines.Engine
	nodeID string
}

// hedgePickWorkers returns up to `n` least-loaded workers that host
// the model, skipping cooldowns + stale heartbeats. Returns the
// chosen candidates in arbitrary order.
func (r *Router) hedgePickWorkers(ctx context.Context, model string, n int) []hedgeCandidate {
	if model == "" || r.store == nil {
		return nil
	}
	placements, err := r.store.Placements().GetByModel(ctx, model)
	if err != nil || len(placements) == 0 {
		return nil
	}
	r.mu.RLock()
	sort.Slice(placements, func(i, j int) bool {
		return r.inflight[placements[i].NodeID] < r.inflight[placements[j].NodeID]
	})
	r.mu.RUnlock()

	out := make([]hedgeCandidate, 0, n)
	for _, p := range placements {
		if len(out) >= n {
			break
		}
		if p.NodeID == r.localNode {
			// Add local as one of the candidates.
			out = append(out, hedgeCandidate{engine: r.local, nodeID: p.NodeID})
			continue
		}
		node, err := r.store.Nodes().Get(ctx, p.NodeID)
		if err != nil || node == nil || node.Address == "" {
			continue
		}
		if r.heartbeatMaxAge > 0 && !node.LastHeartbeat.IsZero() &&
			time.Since(node.LastHeartbeat) > r.heartbeatMaxAge {
			continue
		}
		if r.inCooldown(node.ID) {
			continue
		}
		out = append(out, hedgeCandidate{
			engine: r.getOrCreateRemote(node.ID, node.Address, node.WorkerToken),
			nodeID: node.ID,
		})
	}
	return out
}

// drainWithTimeout consumes a stream channel for at most `d`, then
// returns. Used when the client disconnects mid-stream: we cancel the
// engine context to stop the producer, then drain whatever's already
// buffered. Without the timeout, a hung backend would leak a goroutine
// blocked on a receive that never completes.
func drainWithTimeout[T any](ch <-chan T, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-t.C:
			return
		}
	}
}

// logFallback emits a structured slog event so operators can filter
// fallback activations by model, fallback target, op, or error class.
// The metric (`opod_router_fallback_total{op, reason}`) is now bumped
// by the call site so the reason label can distinguish per-request
// overrides from catalog-driven fallbacks.
func (r *Router) logFallback(primary, used, op string, primaryErr error) {
	if r.log != nil {
		r.log.Warn("router fallback",
			"op", op,
			"primary", primary,
			"used", used,
			"err", primaryErr,
		)
	}
}

// pick returns an engine and the node id it represents.
