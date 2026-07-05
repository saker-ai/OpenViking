// Package retrieve — telemetry: latency, recall, level breakdown.
//
// The retrieve pipeline emits one OpenTelemetry span per Retrieve call
// and one child span per L0/L1/L2 stage. The StatsCollector here is a
// lightweight, dependency-free wrapper around otel's tracer so the
// retrieve package can be tested without spinning up a real exporter.
//
// In production, the spans are exported via the global tracer provider
// configured by internal/observability.InitTracer. In tests, the
// "memory" exporter mode means spans are dropped silently.
package retrieve

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// tracerName is the OTel tracer name used by the retrieve package.
const tracerName = "github.com/saker-ai/ctxhub/internal/retrieve"

// StatsCollector records per-call latency and level breakdown. It is
// safe for concurrent use.
type StatsCollector struct {
	tracer trace.Tracer
}

// NewStatsCollector constructs a StatsCollector bound to the global OTel
// tracer. In tests, the global tracer is the no-op tracer (default), so
// spans are dropped without overhead.
func NewStatsCollector() *StatsCollector {
	return &StatsCollector{tracer: otel.GetTracerProvider().Tracer(tracerName)}
}

// StartSpan starts a child span for the given operation name. The
// returned context should be passed to downstream calls; the returned
// function must be called exactly once to end the span (defer it).
func (c *StatsCollector) StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, func(err error)) {
	if c == nil || c.tracer == nil {
		return ctx, func(_ error) {}
	}
	ctx, span := c.tracer.Start(ctx, name, trace.WithAttributes(attrs...))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		} else {
			span.SetStatus(codes.Ok, "")
		}
		span.End()
	}
}

// LevelSpan is a convenience helper that starts a span for a level
// stage (L0/L1/L2) and returns the level-specific latency.
func (c *StatsCollector) LevelSpan(ctx context.Context, level Level, op string) (context.Context, func(err error)) {
	attrs := []attribute.KeyValue{
		attribute.String("retrieve.level", string(level)),
		attribute.String("retrieve.op", op),
	}
	return c.StartSpan(ctx, fmt.Sprintf("retrieve.%s.%s", level, op), attrs...)
}

// Aggregator accumulates per-call stats in memory. The retrieve pipeline
// uses one Aggregator per Retrieve call; the final Stats object is built
// from it on return.
type Aggregator struct {
	mu         sync.Mutex
	start      time.Time
	byLevel    map[Level]time.Duration
	hits       map[Level]int
	candidates int // unique candidate URIs after L0
	final      int // unique URIs after L2/rerank
}

// NewAggregator starts a fresh aggregator at the given instant.
func NewAggregator(now time.Time) *Aggregator {
	return &Aggregator{
		start:   now,
		byLevel: map[Level]time.Duration{},
		hits:    map[Level]int{},
	}
}

// RecordLevel adds a stage's elapsed time and hit count.
func (a *Aggregator) RecordLevel(level Level, elapsed time.Duration, hits int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.byLevel[level] += elapsed
	a.hits[level] += hits
}

// RecordCandidates sets the candidate count for recall estimation.
func (a *Aggregator) RecordCandidates(afterL0, afterFinal int) {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.candidates = afterL0
	a.final = afterFinal
}

// Stats returns the snapshot. now is the call's end time.
func (a *Aggregator) Stats(now time.Time) *Stats {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s := &Stats{
		Latency:        now.Sub(a.start),
		LatencyByLevel: make(map[Level]time.Duration, len(a.byLevel)),
		HitsByLevel:    make(map[Level]int, len(a.hits)),
	}
	for k, v := range a.byLevel {
		s.LatencyByLevel[k] = v
	}
	for k, v := range a.hits {
		s.HitsByLevel[k] = v
	}
	if a.candidates > 0 {
		s.RecallEstimate = float64(a.final) / float64(a.candidates)
	}
	return s
}
