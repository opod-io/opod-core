package router

import (
	"context"
	"time"

	"github.com/opod-io/opod/internal/metrics"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func (r *Router) maybeSwapTypedChain(chain []string, chains FallbackChains, err error, op string, primaryIdx int, span trace.Span) []string {
	class := ClassifyError(err)
	span.SetAttributes(attribute.String("opod.fallback.classifier", class.String()))
	if class == ClassGeneric {
		return chain
	}
	typed := chains.PickFor(class)
	if len(typed) == 0 || sameOrder(typed, chains.Generic) {
		return chain
	}
	// Replace the remainder of the chain with the typed list. We do not
	// rebuild via buildChain here because the primary has already been
	// tried — typed lists are pure replacements for the *fallbacks*.
	newChain := make([]string, 0, primaryIdx+1+len(typed))
	newChain = append(newChain, chain[:primaryIdx+1]...)
	newChain = append(newChain, typed...)
	newChain = r.applyCap(newChain)
	metrics.ObserveRouterFallback(op, class.String())
	if r.log != nil {
		r.log.Info("router typed fallback",
			"op", op,
			"classifier", class.String(),
			"new_chain_len", len(newChain),
		)
	}
	return newChain
}

// inCooldown reports whether the named node is currently in the
// penalty box. Cheap, takes RLock — pick() checks this on every worker.
func (r *Router) chainFor(model string, ov Overrides) ([]string, string, FallbackChains) {
	if len(ov.Fallbacks) > 0 {
		chain := make([]string, 0, len(ov.Fallbacks)+1)
		chain = append(chain, model)
		chain = append(chain, ov.Fallbacks...)
		return chain, "request", FallbackChains{}
	}
	chains := r.chainsFor(model)
	return r.applyCap(buildChain(model, chains.Generic)), "catalog", chains
}

// waitBackoff sleeps for the backoff interval before retry attempt `n`.
// Initial backoff doubles each retry, capped at RetryBackoffCapMS.
// Respects context cancellation — returns ctx.Err() if the caller went
// away while we were waiting.
func waitBackoff(ctx context.Context, retry, initialMS int) error {
	if initialMS <= 0 {
		return nil
	}
	delay := initialMS
	for i := 1; i < retry; i++ {
		delay *= 2
		if delay > RetryBackoffCapMS {
			delay = RetryBackoffCapMS
			break
		}
	}
	t := time.NewTimer(time.Duration(delay) * time.Millisecond)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Chat dispatches a chat request, with optional fallback. Tries the primary
// model first; on synchronous error from Engine.Chat (the engine couldn't
// even start the stream), walks the fallback chain in order. Once the
// stream starts producing events, fallback is no longer possible —
// downstream errors propagate as-is.
//
// Tracing note: router.Chat starts a span at request entry. Each fallback
// attempt is a child span. The span covering the eventual successful
// candidate stays open across the streaming relay and is closed by the
// goroutine that drains the inner stream — so its duration matches actual
// time-to-completion, not just the time to start the stream.
