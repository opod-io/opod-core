package leader

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/opod-io/opod-core/internal/usage"
)

// initTracing wires an OTLP/HTTP trace exporter against endpoint and returns
// a shutdown func the caller must invoke at process exit. When endpoint is
// empty, returns a no-op shutdown — tracing stays disabled with zero runtime
// overhead beyond the (cheap) NoopTracerProvider.
//
// Endpoint forms accepted (otlpTarget):
//
//	"http://otel-collector:4318"            — explicit URL, spans to /v1/traces
//	"otel-collector:4318"                   — bare host:port (defaults to http://)
//	"https://collector.example.com"         — TLS
//	"https://gw.example.com/otel"           — a path prefix: /otel/v1/traces
//	"https://gw.example.com/otel/v1/traces" — the full signal URL, as written
//
// headers is OPOD_OTLP_HEADERS ("k=v,k=v"), sent on every export — the
// collector's token. An endpoint or header value it cannot use turns tracing
// off with a warning naming it; the leader still starts.
//
// sampleRatio is OPOD_TRACE_SAMPLE_RATIO (traceSampler): checked even when
// tracing is off, so a typo fails the start where it was made rather than the
// day a collector is connected.
//
// Inserts a default 5s export timeout so a slow collector can't pin the
// process. Tracer name is "github.com/opod-io/opod-core".
func initTracing(ctx context.Context, endpoint, headers, sampleRatio, version string, log *slog.Logger) (func(context.Context) error, error) {
	sampler, ratio, err := traceSampler(sampleRatio)
	if err != nil {
		return nil, err
	}
	if endpoint == "" {
		// Set propagator so any incoming traceparent headers flow through the
		// service, even when we're not exporting locally — useful when Opod
		// sits between two services that both export upstream.
		otel.SetTextMapPropagator(propagation.TraceContext{})
		return func(context.Context) error { return nil }, nil
	}

	t, err := otlpTarget(endpoint, headers, "/v1/traces")
	if err != nil {
		// Telemetry never decides whether the leader starts (as log export
		// does): an endpoint or header it cannot use turns tracing off, loudly.
		otel.SetTextMapPropagator(propagation.TraceContext{})
		log.Warn("tracing disabled: the OTLP trace settings cannot be used", "err", err)
		return func(context.Context) error { return nil }, nil
	}

	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpoint(t.host),
		otlptracehttp.WithURLPath(t.path),
		otlptracehttp.WithTimeout(5 * time.Second),
	}
	if t.scheme == "http" {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	if len(t.headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(t.headers))
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
		tracesdk.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	// Header names only: a value is the collector's credential.
	log.Info("tracing enabled", "endpoint", endpoint, "scheme", t.scheme, "path", t.path, "headers", t.headerNames(), "sample_ratio", ratio)

	return tp.Shutdown, nil
}

// traceSampler reads OPOD_TRACE_SAMPLE_RATIO (PLAN T32.8): the share of
// traces this leader STARTS that it records, from 0 to 1. Unset = 1, every
// request, as before the variable existed. Parent-based: a request that
// arrives with a traceparent follows its caller's decision — a sampled parent
// (LiteLLM in front of opod) is always recorded, an unsampled one never — so
// a trace is never cut in the middle. Only a root span is decided by the
// ratio, by its trace id, so every hop that samples by ratio keeps the same
// traces. An unreadable value is an error naming the variable.
func traceSampler(raw string) (tracesdk.Sampler, float64, error) {
	ratio := 1.0
	if v := strings.TrimSpace(raw); v != "" {
		r, err := strconv.ParseFloat(v, 64)
		if err != nil || !(r >= 0 && r <= 1) { // written so NaN is refused: it fails every comparison
			return nil, 0, fmt.Errorf("OPOD_TRACE_SAMPLE_RATIO=%q: want a number from 0 to 1", raw)
		}
		ratio = r
	}
	return tracesdk.ParentBased(tracesdk.TraceIDRatioBased(ratio)), ratio, nil
}

// otlpDest is where an OTLP/HTTP exporter sends one signal, and what it
// sends with it.
type otlpDest struct {
	host, scheme, path string
	headers            map[string]string
}

// otlpTarget reads a collector endpoint and OPOD_OTLP_HEADERS for one signal.
// The URL's path is kept, and the signal's path (signalPath, "/v1/traces" or
// "/v1/logs") is added after it unless it already ends in it: none or "/"
// posts to /v1/logs, a prefix "/otel" to /otel/v1/logs, "/otel/v1/logs" as
// written. That is the rule the control plane's own OTLP exporters and its
// Verify follow, so one saved URL — a collector behind a path prefix —
// reaches the same place from the control plane and from a leader. A bare
// host:port (no scheme) is plain http, as before; it may carry a path too.
//
// headers is "k=v,k=v" (the OTEL_EXPORTER_OTLP_HEADERS shape, parsed by the
// same rule as OPOD_USAGE_OTLP_HEADERS): a value may hold '=' but not ',', and
// is sent as written — no percent-decoding.
func otlpTarget(endpoint, headers, signalPath string) (otlpDest, error) {
	raw := strings.TrimSpace(endpoint)
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw // bare host:port — most local collectors run plaintext
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return otlpDest{}, fmt.Errorf("%q: want http(s)://host[:port][/path] or host:port", endpoint)
	}
	path := strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(path, signalPath) {
		path += signalPath
	}
	h, err := usage.ParseHeaders(headers)
	if err != nil {
		return otlpDest{}, fmt.Errorf("OPOD_OTLP_HEADERS: %w", err)
	}
	return otlpDest{host: u.Host, scheme: u.Scheme, path: path, headers: h}, nil
}

// headerNames lists the header names, sorted, for a log line that must not
// carry their values.
func (t otlpDest) headerNames() []string {
	names := make([]string, 0, len(t.headers))
	for k := range t.headers {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}
