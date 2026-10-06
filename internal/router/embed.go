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

// Embed routes one embeddings request: the catalog chain, the retries, the
// next worker on an unreachable one — everything Chat does but stream
// (unary, below).
func (r *Router) Embed(ctx context.Context, req engines.EmbedRequest) (engines.EmbedResponse, error) {
	var res engines.EmbedResponse
	err := r.unary(ctx, embedOp, req.Model, unaryCall{
		supported: func(eng engines.Engine) bool { _, ok := eng.(engines.EmbedEngine); return ok },
		do: func(ctx context.Context, eng engines.Engine, model string) error {
			attempt := req
			attempt.Model = model
			out, err := eng.(engines.EmbedEngine).Embed(ctx, attempt)
			if err == nil {
				res = out
			}
			return err
		},
	})
	return res, err
}

var embedOp = unaryOp{
	span:   "Embed",
	metric: "embed",
	unsupported: func(engine string) error {
		return fmt.Errorf("engine %s does not support embeddings", engine)
	},
}

// unaryOp names a non-streaming request kind for the spans, the metrics and
// the refusal of an engine that cannot serve it.
type unaryOp struct {
	span        string // "Embed" → spans router.Embed and router.Embed.attempt
	metric      string // the op label on opod_router_fallback_total
	unsupported func(engine string) error
}

// unaryCall is one request kind's half of the dispatch: whether an engine can
// serve it at all, and the call itself under the model name the chosen engine
// serves.
type unaryCall struct {
	supported func(engines.Engine) bool
	do        func(ctx context.Context, eng engines.Engine, model string) error
}

// unary is the one dispatch loop behind every non-streaming request kind —
// embeddings and rerank (ADR-084) — so the second kind does not grow a second
// copy of the chain walk, the retries and the next-worker rule. It returns the
// first candidate's error when every candidate failed, as Embed always did.
func (r *Router) unary(ctx context.Context, op unaryOp, model string, call unaryCall) error {
	ctx, span := tracer.Start(ctx, "router."+op.span,
		trace.WithAttributes(
			attribute.String("opod.model.requested", model),
		),
	)
	defer span.End()

	ctx = withPickState(ctx) // what this request remembers between picks (nextworker.go)
	ov := FromContext(ctx)
	chain, source, chains := r.chainFor(model, ov)
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
		attemptModel := candidate
		attemptStart := time.Now()

		attemptCtx, attemptSpan := tracer.Start(ctx, "router."+op.span+".attempt",
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
			if i == 0 && primaryErr == nil { // a worker's own error, when there was one, says more than "none left"
				primaryErr = err
			}
			continue
		}
		NoteNode(attemptCtx, nodeID) // usage attribution: the worker this attempt dispatches to
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
			attemptModel = r.localResolve(candidate)
		}
		if !call.supported(eng) {
			err := op.unsupported(eng.Name())
			attemptSpan.SetStatus(codes.Error, "engine missing "+op.span)
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
				metrics.ObserveRouterFallback(op.metric, "retry")
			}
			r.incInflight(nodeID, candidate)
			err := call.do(attemptCtx, eng, attemptModel)
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
					r.logFallback(model, candidate, op.metric, primaryErr)
					metrics.ObserveRouterFallback(op.metric, reason)
					span.SetAttributes(attribute.Int("opod.fallback.used_at", i))
				}
				span.SetAttributes(attribute.String("opod.model.served", candidate))
				r.latency.record(candidate, time.Since(attemptStart), 0)
				return nil
			}
			lastErr = err
		}
		attemptSpan.SetStatus(codes.Error, op.metric+" failed")
		attemptSpan.RecordError(lastErr)
		attemptSpan.End()
		if i == 0 && primaryErr == nil {
			primaryErr = lastErr
		}
		// The worker was unreachable and got nothing: the same model, another worker.
		if next, again := r.tryNextWorker(ctx, nodeID, lastErr); again {
			ctx = next
			metrics.ObserveRouterFallback(op.metric, "next-worker")
			i--
			continue
		}
		// After the first candidate fails, classify the error and swap
		// the rest of the chain for a typed list when one is configured
		// AND it differs from the generic list. Only applies to
		// catalog-driven routing; per-request overrides bypass typed
		// selection on the theory that the operator explicitly chose
		// their chain.
		if !classified && source == "catalog" {
			classified = true
			chain = r.maybeSwapTypedChain(chain, chains, lastErr, op.metric, i, span)
		}
	}
	span.SetStatus(codes.Error, "all candidates failed")
	if primaryErr != nil {
		span.RecordError(primaryErr)
	}
	return primaryErr
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
