// Package observability wires OpenTelemetry tracing, Prometheus metrics,
// and structured logging. The tracer provider returned by InitTracer is
// shutdown-aware so callers can flush spans on graceful exit.
package observability

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"github.com/saker-ai/ctxhub/internal/config"
)

// ShutdownFunc flushes and stops the tracer provider. Callers must invoke
// it on graceful exit; the returned error is non-nil only when flush fails.
type ShutdownFunc func(context.Context) error

// InitTracer configures a global tracer provider matching cfg.OTEL.
//
// Exporters:
//   - "memory":     no-op provider (default for tests/dev)
//   - "otlp-grpc":  OTLP gRPC exporter to cfg.Endpoint
//   - "otlp-http":  OTLP HTTP exporter to cfg.Endpoint
//
// The returned ShutdownFunc is safe to call when nil; callers should
// still defer it.
func InitTracer(ctx context.Context, cfg config.OTELConfig) (ShutdownFunc, error) {
	res := resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(cfg.ServiceName),
	)

	var exp sdktrace.SpanExporter
	switch cfg.Exporter {
	case "", "memory":
		// No-op: spans are dropped. Replaced in P13 with an in-memory
		// recorder for tests and a real exporter for prod.
		return func(context.Context) error { return nil }, nil
	case "otlp-grpc":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithTimeout(10 * time.Second)}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracegrpc.WithEndpoint(cfg.Endpoint))
		}
		e, err := otlptracegrpc.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("otel: otlp-grpc: %w", err)
		}
		exp = e
	case "otlp-http":
		opts := []otlptracehttp.Option{otlptracehttp.WithTimeout(10 * time.Second)}
		if cfg.Endpoint != "" {
			opts = append(opts, otlptracehttp.WithEndpoint(cfg.Endpoint))
		}
		e, err := otlptracehttp.New(ctx, opts...)
		if err != nil {
			return nil, fmt.Errorf("otel: otlp-http: %w", err)
		}
		exp = e
	default:
		return nil, fmt.Errorf("otel: unsupported exporter %q", cfg.Exporter)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(
			sdktrace.TraceIDRatioBased(cfg.SampleRate),
		)),
	)
	otel.SetTracerProvider(tp)

	return func(ctx context.Context) error {
		shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		return tp.Shutdown(shutdownCtx)
	}, nil
}
