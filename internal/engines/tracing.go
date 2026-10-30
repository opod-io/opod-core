package engines

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// tracer is shared so every engine driver reports under the same
// instrumentation-library name. The global TracerProvider (set in
// internal/leader/tracing.go) decides whether spans are exported
// or NoOp'd, so there's zero overhead when OTLP isn't configured.
var tracer trace.Tracer = otel.Tracer("github.com/opod-io/opod/internal/engines")

// StartChatSpan opens a "<driver>.Chat" span with the standard attributes
// every Opod engine driver records: driver name, model id, target
// endpoint, message count. The returned span is closed by the caller — by
// convention drivers `defer span.End()` inside the streaming goroutine so
// the span duration covers the full streamed response, not just the
// time-to-first-byte.
//
// Usage:
//
//	ctx, sp := engines.StartChatSpan(ctx, "vllm", req.Model, v.endpoint, len(req.Messages))
//	// ... synchronous errors → sp.MarkError("...", err); sp.End(); return
//	go func() {
//	    defer sp.End()
//	    ...
//	    sp.SetTokens(promptTokens, completionTokens)
//	    sp.SetStatus(codes.Ok, "")
//	}()
func StartChatSpan(ctx context.Context, driver, model, endpoint string, messageCount int) (context.Context, *ChatSpan) {
	ctx, sp := tracer.Start(ctx, driver+".Chat",
		trace.WithAttributes(
			attribute.String("opod.engine", driver),
			attribute.String("opod.model", model),
			attribute.String("opod.engine.endpoint", endpoint),
			attribute.Int("opod.messages", messageCount),
		),
	)
	return ctx, &ChatSpan{Span: sp}
}

// ChatSpan wraps trace.Span with the helpers drivers set per request — the
// HTTP status from the engine response and the prompt + completion token
// counts from the final stream event.
type ChatSpan struct{ trace.Span }

// SetHTTPStatus records the engine's HTTP response code.
func (s *ChatSpan) SetHTTPStatus(code int) {
	s.SetAttributes(attribute.Int("http.status_code", code))
}

// SetTokens records the final prompt/completion token counts.
func (s *ChatSpan) SetTokens(prompt, completion int) {
	s.SetAttributes(
		attribute.Int("opod.tokens.prompt", prompt),
		attribute.Int("opod.tokens.completion", completion),
	)
}

// MarkError tags the span failed and records err (when non-nil) in one call.
func (s *ChatSpan) MarkError(msg string, err error) {
	s.SetStatus(codes.Error, msg)
	if err != nil {
		s.RecordError(err)
	}
}

// SetCachedTokens records how many prompt tokens the engine served from its
// prefix cache, when it reported a count (usage.prompt_tokens_details.
// cached_tokens; vLLM, SGLang, llama-server). 0 = not reported, and nothing is
// set: a span cannot tell "no hit" from "the engine does not say".
func (s *ChatSpan) SetCachedTokens(cached int) {
	if cached > 0 {
		s.SetAttributes(attribute.Int("opod.tokens.cached", cached))
	}
}

// Trace continuation across the engine hop (PLAN T32.7).
//
// A trace that reaches the leader with a W3C traceparent (LiteLLM in front of
// opod sends one) must go on into the engine, which can export its own spans
// under it. Every HTTP call a driver makes goes through TraceTransport, so the
// header is set in one place for every driver, and on every hop: leader →
// worker (the leader's client for a worker is the OpenAI-wire driver) and
// worker → engine. A worker exports no spans of its own; ContinueTrace reads
// the leader's header on the way in so the worker's engine call carries it on.
// The leader's other calls for a request use it too (T32.42): load and unload
// (scheduler.New), sleep, resume and tokenize (leader.workerHTTP) and the
// fallback forward.
//
// Header only: no client span and nothing per chunk — engine tracing is the
// engine's. The propagator is W3C trace context, named here rather than read
// from the global one, because a worker never sets a global propagator and
// both ends must behave the same.
var w3c = propagation.TraceContext{}

// TraceTransport wraps base (nil = http.DefaultTransport) so a request whose
// context carries a valid span context leaves with that context's traceparent
// (and tracestate). A request without one — a health probe, a metrics scrape —
// goes out untouched, with no copy made.
func TraceTransport(base http.RoundTripper) *TracedTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &TracedTransport{Base: base}
}

// TracedTransport is TraceTransport's round tripper. Base is the transport
// that does the work (its dial, idle and TLS settings).
type TracedTransport struct{ Base http.RoundTripper }

// RoundTrip satisfies http.RoundTripper.
func (t *TracedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !trace.SpanContextFromContext(req.Context()).IsValid() {
		return t.Base.RoundTrip(req)
	}
	// A RoundTripper must not modify the request it was given: copy it with
	// its own header map, then inject into the copy.
	out := *req
	out.Header = req.Header.Clone()
	if out.Header == nil {
		out.Header = http.Header{}
	}
	w3c.Inject(req.Context(), propagation.HeaderCarrier(out.Header))
	return t.Base.RoundTrip(&out)
}

// CloseIdleConnections passes through to the wrapped transport. Without it
// http.Client.CloseIdleConnections finds no closer on the wrapper and does
// nothing, and an evicted engine's pooled sockets live on (PLAN T15.16).
func (t *TracedTransport) CloseIdleConnections() {
	if c, ok := t.Base.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// ContinueTrace makes a request's W3C traceparent, when it has one, the parent
// span context of the request's context, so an engine call made under it
// carries the same trace on (TraceTransport). The worker's API runs under it;
// the leader's own listener does the same through otelhttp.
func ContinueTrace(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("traceparent") != "" {
			r = r.WithContext(w3c.Extract(r.Context(), propagation.HeaderCarrier(r.Header)))
		}
		next.ServeHTTP(w, r)
	})
}
