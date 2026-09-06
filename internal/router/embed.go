package router

import (
	"context"
	"fmt"
	"time"

	"github.com/opod-io/opod/internal/engines"
	"github.com/opod-io/opod/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

func (r *Router) Embed(ctx context.Context, req engines.EmbedRequest) (engines.EmbedResponse, error) {
	ctx, span := tracer.Start(ctx, "router.Embed",
		trace.WithAttributes(
			attribute.String("opod.model.requested", req.Model),
		),
	)
	defer span.End()

	ov := FromContext(ctx)
	chain, source, chains := r.chainFor(req.Model, ov)
	switch {
	case ov.Sort != "":
		// Explicit per-request sort wins over the latency-pressure
		// reorder — the client asked for a specific metric.
		chain = r.sortChain(chain, ov.Sort)
		span.SetAttributes(attribute.String("opod.sort", ov.Sort))
	case source == "catalog":
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

		attemptCtx, attemptSpan := tracer.Start(ctx, "router.Embed.attempt",
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
		ee, ok := eng.(engines.EmbedEngine)
		if !ok {
			err := fmt.Errorf("engine %s does not support embeddings", eng.Name())
			attemptSpan.SetStatus(codes.Error, "engine missing Embed")
			attemptSpan.RecordError(err)
			attemptSpan.End()
			if i == 0 {
				primaryErr = err
			}
			continue
		}
		// Retry loop wraps the single candidate. Each retry produces a
		// child span so traces show the wall-clock cost cleanly.
		var lastErr error
		for retry := 0; retry <= ov.NumRetries; retry++ {
			if retry > 0 {
				if err := waitBackoff(attemptCtx, retry, ov.RetryBackoffMS); err != nil {
					lastErr = err
					break
				}
				metrics.ObserveRouterFallback("embed", "retry")
			}
			r.incInflight(nodeID, candidate)
			res, err := ee.Embed(attemptCtx, attempt)
			r.decInflight(nodeID, candidate)
			r.recordOutcome(nodeID, err == nil)
			if err == nil {
				// Pin (user, model)→node on success — same as Chat.
				r.rememberSticky(ctx, candidate, nodeID)
				attemptSpan.SetStatus(codes.Ok, "")
				attemptSpan.End()
				if i > 0 {
					reason := "primary-error"
					if source == "request" {
						reason = "per-request"
					}
					r.logFallback(req.Model, candidate, "embed", primaryErr)
					metrics.ObserveRouterFallback("embed", reason)
					span.SetAttributes(attribute.Int("opod.fallback.used_at", i))
				}
				span.SetAttributes(attribute.String("opod.model.served", candidate))
				r.latency.record(candidate, time.Since(attemptStart), 0)
				return res, nil
			}
			lastErr = err
		}
		attemptSpan.SetStatus(codes.Error, "embed failed")
		attemptSpan.RecordError(lastErr)
		attemptSpan.End()
		if i == 0 {
			primaryErr = lastErr
		}
		// After the first candidate fails, classify the error and swap
		// the rest of the chain for a typed list when one is configured
		// AND it differs from the generic list. Only applies to
		// catalog-driven routing; per-request overrides bypass typed
		// selection on the theory that the operator explicitly chose
		// their chain.
		if !classified && source == "catalog" {
			classified = true
			chain = r.maybeSwapTypedChain(chain, chains, lastErr, "embed", i, span)
		}
	}
	span.SetStatus(codes.Error, "all candidates failed")
	if primaryErr != nil {
		span.RecordError(primaryErr)
	}
	return engines.EmbedResponse{}, primaryErr
}

// maybeSwapTypedChain inspects `err`, classifies it, and (if the
// classifier returned a typed bucket with a non-empty list that differs
// from generic) replaces the remainder of `chain` from index i+1
// onwards with the typed list.
//
// Returns the (possibly new) chain. Emits the
// `opod.fallback.classifier` span attribute and the
// `opod_router_fallback_total{reason="content-policy|context-length"}`
// metric so operators can see which classifier branch fired.
