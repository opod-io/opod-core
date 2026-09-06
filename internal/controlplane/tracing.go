package controlplane

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// initTracing wires an OTLP/HTTP trace exporter against endpoint and returns
// a shutdown func the caller must invoke at process exit. When endpoint is
// empty, returns a no-op shutdown — tracing stays disabled with zero runtime
// overhead beyond the (cheap) NoopTracerProvider.
//
// Endpoint forms accepted:
//
//	"http://otel-collector:4318"        — explicit URL
//	"otel-collector:4318"               — bare host:port (defaults to http://)
//	"https://collector.example.com"     — TLS
//
// Inserts a default 5s export timeout so a slow collector can't pin the
// process. Tracer name is "github.com/opod-io/opod".
func initTracing(ctx context.Context, endpoint, version string, log *slog.Logger) (func(context.Context) error, error) {
	if endpoint == "" {
		// Set propagator so any incoming traceparent headers flow through the
		// service, even when we're not exporting locally — useful when Opod
		// sits between two services that both export upstream.
		otel.SetTextMapPropagator(propagation.TraceContext{})
		return func(context.Context) error { return nil }, nil
	}

	host, scheme, err := parseOTLPEndpoint(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse otlp endpoint: %w", err)
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(host),
		otlptracehttp.WithTimeout(5 * time.Second),
	}
	if scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceName("opod"),
			attribute.String("service.version", version),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}

	tp := tracesdk.NewTracerProvider(
		tracesdk.WithBatcher(exporter, tracesdk.WithBatchTimeout(2*time.Second)),
		tracesdk.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	log.Info("tracing enabled", "endpoint", endpoint, "scheme", scheme)

	return tp.Shutdown, nil
}

// parseOTLPEndpoint accepts URL or bare host:port, returns (host, scheme).
func parseOTLPEndpoint(ep string) (host, scheme string, err error) {
	if u, perr := url.Parse(ep); perr == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") {
		return u.Host, u.Scheme, nil
	}
	// Bare host:port form — default to http (most local collectors run plaintext).
	return ep, "http", nil
}
