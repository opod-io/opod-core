package controlplane

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	logsdk "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// Log export: the leader's own records over OTLP/HTTP, the way its spans
// already go (tracing.go). Same endpoint forms, same resource, same rule that
// telemetry is never on anyone's critical path:
//
//   - the local handler keeps working — export is a tee, not a redirect, so
//     `kubectl logs`, journald and a terminal see exactly what they saw before;
//   - the queue is bounded and emit never blocks: a collector that is slow or
//     gone costs the oldest queued records, not a request;
//   - nothing here logs through slog, so a failing sink cannot feed itself.
//
// Only this process's records are exported. What an engine writes to its own
// stdout belongs to whatever collects the host's or the pod's logs.

const (
	logExportQueue    = 2048            // records held while the collector is slow; beyond it the oldest are dropped
	logExportInterval = 2 * time.Second // batch flush, the same cadence as spans
	logExportTimeout  = 5 * time.Second // one export call; a hung collector cannot hold the batcher
)

// ExportLogs returns a logger that writes to base's handler AND exports every
// record to the OTLP/HTTP collector at endpoint, plus the shutdown that flushes
// what is queued. An empty endpoint returns base unchanged and a no-op
// shutdown: export off means no provider, no goroutine, no overhead.
func ExportLogs(ctx context.Context, endpoint, version string, base *slog.Logger) (*slog.Logger, func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if endpoint == "" {
		return base, noop, nil
	}
	host, scheme, err := parseOTLPEndpoint(endpoint)
	if err != nil {
		return base, noop, fmt.Errorf("parse otlp logs endpoint: %w", err)
	}
	opts := []otlploghttp.Option{
		otlploghttp.WithEndpoint(host),
		otlploghttp.WithTimeout(logExportTimeout),
	}
	if scheme == "http" {
		opts = append(opts, otlploghttp.WithInsecure())
	}
	exporter, err := otlploghttp.New(ctx, opts...)
	if err != nil {
		return base, noop, fmt.Errorf("create OTLP log exporter: %w", err)
	}
	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName("opod"),
		attribute.String("service.version", version),
	))
	if err != nil {
		return base, noop, fmt.Errorf("build resource: %w", err)
	}
	provider := logsdk.NewLoggerProvider(
		logsdk.WithResource(res),
		logsdk.WithProcessor(logsdk.NewBatchProcessor(exporter,
			logsdk.WithMaxQueueSize(logExportQueue),
			logsdk.WithExportInterval(logExportInterval),
			logsdk.WithExportTimeout(logExportTimeout),
		)),
	)
	exported := otelslog.NewHandler("github.com/opod-io/opod", otelslog.WithLoggerProvider(provider))
	log := slog.New(teeHandler{local: base.Handler(), export: exported})
	log.Info("log export enabled", "endpoint", endpoint, "scheme", scheme)
	return log, provider.Shutdown, nil
}

// teeHandler writes each record locally and exports it. The local handler's
// level governs both: `log_level` is one setting, and a collector must not
// receive debug records the operator did not ask for. (slog.NewMultiHandler,
// go 1.26, enables on ANY handler, and the bridge accepts every level.)
type teeHandler struct{ local, export slog.Handler }

func (t teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return t.local.Enabled(ctx, level)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	return errors.Join(t.local.Handle(ctx, r.Clone()), t.export.Handle(ctx, r))
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{t.local.WithAttrs(attrs), t.export.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{t.local.WithGroup(name), t.export.WithGroup(name)}
}
