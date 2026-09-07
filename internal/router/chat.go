package router

import (
	"context"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func (r *Router) Chat(ctx context.Context, req engines.ChatRequest) (<-chan engines.StreamEvent, error) {
	// Hedging short-circuit: when the request opts in and the router
	// is configured for replicas > 1, fire concurrent calls and
	// return whichever stream opens first. Retry / fallback are
	// skipped — the operator chose the N× cost trade.
	if ov := FromContext(ctx); ov.Hedge && r.hedgeReplicas > 1 {
		if stream, err := r.chatHedged(ctx, req, r.hedgeReplicas); stream != nil || err != nil {
			return stream, err
		}
		// fall through to the normal path when hedging found no
		// eligible workers.
	}
	ctx, span := tracer.Start(ctx, "router.Chat",
		trace.WithAttributes(
			attribute.String("opod.model.requested", req.Model),
			attribute.Bool("opod.stream", req.Stream),
		),
	)
	// Note: span.End() is called in the streaming goroutine for the winning
	// candidate (so its duration covers the full streamed response), or
	// inline below if every candidate fails synchronously.

	ov := FromContext(ctx)
	chain, source, chains := r.chainFor(req.Model, ov)
	switch {
	case ov.Sort != "":
		// Explicit per-request sort (`opod.sort` / `:floor` / `:nitro`)
		// wins over the latency-pressure reorder. Applies to per-request
		// chains too — sorting an explicit fallback list is still the
		// client's stated intent.
		chain = r.sortChain(chain, ov.Sort)
		span.SetAttributes(attribute.String("opod.sort", ov.Sort))
	case source == "catalog":
		// Latency-aware reorder applies only to the catalog chain —
		// per-request overrides are operator intent and shouldn't be
		// silently rearranged.
		if reordered, swapped := r.latency.reorderByLatency(chain); swapped {
			chain = reordered
			span.SetAttributes(
				attribute.Bool("opod.latency.reordered", true),
				attribute.String("opod.latency.front", chain[0]),
			)
		}
	}
	span.SetAttributes(
		attribute.Int("opod.fallback.chain_length", len(chain)),
		attribute.String("opod.fallback.source", source),
	)
	if ov.NumRetries > 0 {
		span.SetAttributes(
			attribute.Int("opod.retry.num_retries", ov.NumRetries),
			attribute.Int("opod.retry.backoff_ms", ov.RetryBackoffMS),
		)
	}

	var primaryErr error
	classified := false
	for i := 0; i < len(chain); i++ {
		candidate := chain[i]
		attempt := req
		attempt.Model = candidate
		attemptStart := time.Now()

		attemptCtx, attemptSpan := tracer.Start(ctx, "router.Chat.attempt",
			trace.WithAttributes(
				attribute.Int("opod.attempt", i),
				attribute.String("opod.model.candidate", candidate),
				attribute.Bool("opod.is_fallback", i > 0),
			),
		)

		eng, nodeID, err := r.pick(attemptCtx, candidate)
		if err != nil {
			attemptSpan.SetStatus(codes.Error, "pick failed")
			attemptSpan.RecordError(err)
			attemptSpan.End()
			if i == 0 {
				primaryErr = err
			}
			continue
		}
		noteNode(attemptCtx, nodeID) // usage attribution: the worker this attempt dispatches to
		attemptSpan.SetAttributes(
			attribute.String("opod.engine", eng.Name()),
			attribute.String("opod.node_id", nodeID),
		)
		// pick() matched placements by CATALOG id. Now translate to the name the
		// chosen engine actually serves under, at DISPATCH. The resolver is engine-
		// aware: for ollama-native models it returns the native tag (e.g.
		// "llama3.2:1b") — which ollama serves under — and for others it returns the
		// catalog id, which vLLM workers serve under via --served-model-name. So one
		// resolve step feeds both local and remote workers the right name. (Doing
		// this BEFORE pick was the bug: it made the placement lookup miss.)
		if r.localResolve != nil {
			attempt.Model = r.localResolve(candidate)
		}

		// Retry loop wraps the engine.Chat call. Retries only apply to
		// synchronous start failures (engine couldn't begin the stream).
		// Once a stream is open, mid-stream errors are NOT retried — the
		// client has already begun seeing tokens.
		//
		// streamCancel is the cancel for the cancellable child context
		// of the *successful* stream — handed to the goroutine below.
		// Failed attempts cancel their own child ctx locally so vet (and
		// future readers) can see no context.CancelFunc leaks across the
		// loop boundary.
		var inner <-chan engines.StreamEvent
		var streamCancel context.CancelFunc
		var lastErr error
		for retry := 0; retry <= ov.NumRetries; retry++ {
			if retry > 0 {
				if err := waitBackoff(attemptCtx, retry, ov.RetryBackoffMS); err != nil {
					lastErr = err
					break
				}
				metrics.ObserveRouterFallback("chat", "retry")
			}
			r.incInflight(nodeID, candidate)
			thisCtx, thisCancel := context.WithCancel(attemptCtx)
			s, err := eng.Chat(thisCtx, attempt)
			if err == nil {
				inner = s
				streamCancel = thisCancel
				r.recordOutcome(nodeID, true)
				// Pin (user, model)→node now that the call has succeeded, so
				// the next turn reuses this node's KV cache.
				r.rememberSticky(ctx, candidate, nodeID)
				break // stream opened — stop retrying
			}
			thisCancel()
			r.decInflight(nodeID, candidate)
			r.recordOutcome(nodeID, false)
			lastErr = err
		}
		if inner == nil {
			attemptSpan.SetStatus(codes.Error, "engine.Chat returned synchronously")
			attemptSpan.RecordError(lastErr)
			attemptSpan.End()
			if i == 0 {
				primaryErr = lastErr
			}
			if !classified && source == "catalog" {
				classified = true
				chain = r.maybeSwapTypedChain(chain, chains, lastErr, "chat", i, span)
			}
			continue
		}
		attemptSpan.SetStatus(codes.Ok, "stream started")
		attemptSpan.End()

		if i > 0 {
			reason := "primary-error"
			if source == "request" {
				reason = "per-request"
			}
			r.logFallback(req.Model, candidate, "chat", primaryErr)
			metrics.ObserveRouterFallback("chat", reason)
			span.SetAttributes(attribute.Int("opod.fallback.used_at", i))
		}
		span.SetAttributes(attribute.String("opod.model.served", candidate))

		out := make(chan engines.StreamEvent, 16)
		// Capture once for the closure — `candidate` is the loop var.
		servedModel := candidate
		go func() {
			defer streamCancel() // always release engine's ctx
			defer r.decInflight(nodeID, servedModel)
			defer close(out)
			defer span.End() // duration covers full streamed response
			var tokenCount, completionTokens int
			for ev := range inner {
				if ev.Delta != "" {
					tokenCount++
				}
				if ev.Usage != nil {
					completionTokens = ev.Usage.CompletionTokens
				}
				select {
				case out <- ev:
				case <-ctx.Done():
					span.SetStatus(codes.Error, "client disconnected")
					streamCancel()
					go drainWithTimeout(inner, 30*time.Second)
					return
				}
			}
			span.SetAttributes(attribute.Int("opod.stream.events", tokenCount))
			span.SetStatus(codes.Ok, "")
			// Latency + throughput sample for this model = full attempt-to-
			// done duration. Feeds reorderByLatency and `sort: throughput`.
			// Engine-reported completion tokens preferred; delta-event count
			// is the fallback proxy when the engine omits usage.
			if completionTokens == 0 {
				completionTokens = tokenCount
			}
			r.latency.record(servedModel, time.Since(attemptStart), completionTokens)
		}()
		return out, nil
	}
	span.SetStatus(codes.Error, "all candidates failed")
	if primaryErr != nil {
		span.RecordError(primaryErr)
	}
	span.End()
	return nil, primaryErr
}

// chatHedged fires the request to up to `replicas` least-loaded
// workers concurrently. The first goroutine whose eng.Chat returns
// without error wins; the losers' contexts are cancelled and their
// inflight counters are decremented.
//
// Returns (nil, nil) when hedging found no eligible workers — the
// caller should fall back to the normal pick path. Returns
// (nil, err) when every replica failed synchronously; the caller
// surfaces the err.
//
// Limitations:
//   - Hedging skips the catalog fallback chain, retries, latency
//     reorder, and typed fallback. The operator already accepted the
//     N× cost.
//   - Local-only deployments fall back to a single local call.
