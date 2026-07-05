package retrieve

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStatsCollectorNilSafe(t *testing.T) {
	t.Parallel()
	var c *StatsCollector
	ctx, end := c.StartSpan(context.Background(), "x")
	_ = ctx
	assert.NotPanics(t, func() { end(nil) })
}

func TestStatsCollectorStartSpanEndSpanOK(t *testing.T) {
	t.Parallel()
	c := NewStatsCollector()
	ctx, end := c.StartSpan(context.Background(), "test.op")
	require.NotNil(t, ctx)
	end(nil)             // should not panic
	end(errors.New("x")) // safe to call twice? yes, span.End is idempotent in otel
}

func TestStatsCollectorLevelSpanReturnsContext(t *testing.T) {
	t.Parallel()
	c := NewStatsCollector()
	ctx, end := c.LevelSpan(context.Background(), Level0Abstract, "search")
	require.NotNil(t, ctx)
	end(nil)
}

func TestAggregatorRecordLevel(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := NewAggregator(now)
	a.RecordLevel(Level0Abstract, 10*time.Millisecond, 5)
	a.RecordLevel(Level1Overview, 20*time.Millisecond, 3)
	a.RecordLevel(Level2Chunk, 30*time.Millisecond, 1)
	stats := a.Stats(now.Add(60 * time.Millisecond))
	require.NotNil(t, stats)
	assert.Equal(t, 60*time.Millisecond, stats.Latency)
	assert.Equal(t, 10*time.Millisecond, stats.LatencyByLevel[Level0Abstract])
	assert.Equal(t, 20*time.Millisecond, stats.LatencyByLevel[Level1Overview])
	assert.Equal(t, 30*time.Millisecond, stats.LatencyByLevel[Level2Chunk])
	assert.Equal(t, 5, stats.HitsByLevel[Level0Abstract])
	assert.Equal(t, 3, stats.HitsByLevel[Level1Overview])
	assert.Equal(t, 1, stats.HitsByLevel[Level2Chunk])
}

func TestAggregatorRecallEstimate(t *testing.T) {
	t.Parallel()
	now := time.Now()
	a := NewAggregator(now)
	a.RecordCandidates(10, 5)
	stats := a.Stats(now)
	require.NotNil(t, stats)
	assert.InDelta(t, 0.5, stats.RecallEstimate, 1e-9)
}

func TestAggregatorZeroCandidatesZeroRecall(t *testing.T) {
	t.Parallel()
	a := NewAggregator(time.Now())
	stats := a.Stats(time.Now())
	require.NotNil(t, stats)
	assert.Zero(t, stats.RecallEstimate)
}

func TestAggregatorNilSafe(t *testing.T) {
	t.Parallel()
	var a *Aggregator
	assert.NotPanics(t, func() {
		a.RecordLevel(Level0Abstract, 0, 0)
		a.RecordCandidates(0, 0)
		_ = a.Stats(time.Now())
	})
}

func TestAggregatorAccumulatesAcrossLevels(t *testing.T) {
	t.Parallel()
	a := NewAggregator(time.Now())
	a.RecordLevel(Level0Abstract, 5*time.Millisecond, 1)
	a.RecordLevel(Level0Abstract, 5*time.Millisecond, 2)
	stats := a.Stats(time.Now())
	assert.Equal(t, 10*time.Millisecond, stats.LatencyByLevel[Level0Abstract])
	assert.Equal(t, 3, stats.HitsByLevel[Level0Abstract])
}
