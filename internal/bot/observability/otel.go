// Package observability wraps internal/observability for the bot's
// own spans and logs. It exists so the bot package does not reach
// into the server's observability config; the bot has its own
// OTELConfig and constructs its own tracer.
//
// The wrapper exposes a single Span helper that creates a trace span
// for the given operation and returns a cancel function that ends the
// span. Errors are recorded automatically.
package observability

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/saker-ai/ctxhub/internal/bot/config"
	serverconfig "github.com/saker-ai/ctxhub/internal/config"
	ovobs "github.com/saker-ai/ctxhub/internal/observability"
)

// Bot is the bot-side observability handle. It holds a tracer and a
// logger; both are derived from the bot's OTELConfig.
type Bot struct {
	tracer trace.Tracer
	logger *slog.Logger
}

// New constructs a Bot observability handle. It does NOT call
// ovobs.InitTracer (the bot may share the global tracer provider when
// embedded in the server process). When the bot runs standalone,
// callers should invoke InitTracer separately.
func New(cfg config.OTELConfig) *Bot {
	tracer := otel.GetTracerProvider().Tracer(cfg.ServiceName)
	logger := ovobs.NewLogger(toServerOTEL(cfg))
	return &Bot{tracer: tracer, logger: logger}
}

// toServerOTEL converts the bot's OTELConfig to the server's
// internal/config.OTELConfig so ovobs.NewLogger can consume it.
func toServerOTEL(cfg config.OTELConfig) serverconfig.OTELConfig {
	return serverconfig.OTELConfig{
		Exporter:    cfg.Exporter,
		ServiceName: cfg.ServiceName,
		SampleRate:  cfg.SampleRate,
		LogLevel:    cfg.LogLevel,
	}
}

// SetLogger attaches a logger. Optional; the bot falls back to a
// logger derived from cfg.
func (b *Bot) SetLogger(l *slog.Logger) {
	if l != nil {
		b.logger = l
	}
}

// Logger returns the attached logger.
func (b *Bot) Logger() *slog.Logger {
	if b.logger == nil {
		return slog.Default()
	}
	return b.logger
}

// Span starts a trace span for op. The returned context carries the
// span; the returned end function must be called when the operation
// finishes (typically deferred). Errors passed to end are recorded on
// the span.
func (b *Bot) Span(ctx context.Context, op string) (context.Context, func(err error)) {
	ctx, span := b.tracer.Start(ctx, op)
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// Tracer returns the underlying tracer (for advanced callers).
func (b *Bot) Tracer() trace.Tracer {
	return b.tracer
}

// String returns a compact description for logging.
func (b *Bot) String() string {
	return fmt.Sprintf("bot.observability{tracer: %T}", b.tracer)
}
