package engines

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracer is shared so every engine driver reports under the same
// instrumentation-library name. The global TracerProvider (set in
// internal/controlplane/tracing.go) decides whether spans are exported
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
