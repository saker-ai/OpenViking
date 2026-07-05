package eval

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubMetric is a controllable Metric for runner tests. Score returns
// FixedScore, or 0 when Fail is true.
type stubMetric struct {
	name       string
	FixedScore float64
	Fail       bool
	calls      int32
}

func (m *stubMetric) Name() string { return m.name }
func (m *stubMetric) Score(ctx context.Context, c EvalCase, answer string, contexts []string) (float64, error) {
	atomic.AddInt32(&m.calls, 1)
	if m.Fail {
		return 0, errors.New("metric failed")
	}
	return m.FixedScore, nil
}

func (m *stubMetric) Calls() int { return int(atomic.LoadInt32(&m.calls)) }

// stubFunc returns canned answer + contexts, counts calls, and
// optionally fails.
type stubFunc struct {
	answer    string
	contexts  []string
	err       error
	calls     int32
	maxCalls  int32 // optional: fail after N calls
	failCount int32
}

func (f *stubFunc) call(ctx context.Context, q string) (string, []string, error) {
	n := atomic.AddInt32(&f.calls, 1)
	if f.maxCalls > 0 && n > f.maxCalls {
		atomic.AddInt32(&f.failCount, 1)
		return "", nil, errors.New("function under test failed")
	}
	if f.err != nil {
		return "", nil, f.err
	}
	return f.answer, append([]string(nil), f.contexts...), nil
}

func (f *stubFunc) Calls() int { return int(atomic.LoadInt32(&f.calls)) }

func TestRunner_RequiresMetricsAndFunc(t *testing.T) {
	t.Parallel()
	_, err := (&Runner{}).Run(context.Background(), "ds", []EvalCase{{Query: "q"}})
	require.Error(t, err)
	_, err = (&Runner{Metrics: []Metric{&stubMetric{name: "m"}}}).Run(context.Background(), "ds", []EvalCase{{Query: "q"}})
	require.Error(t, err)
}

func TestRunner_EmptyDatasetErrors(t *testing.T) {
	t.Parallel()
	m := &stubMetric{name: "m", FixedScore: 1}
	r := &Runner{Metrics: []Metric{m}, Func: func(context.Context, string) (string, []string, error) { return "a", nil, nil }}
	_, err := r.Run(context.Background(), "ds", nil)
	require.Error(t, err)
}

func TestRunner_HappyPath(t *testing.T) {
	t.Parallel()
	m1 := &stubMetric{name: "m1", FixedScore: 1.0}
	m2 := &stubMetric{name: "m2", FixedScore: 0.5}
	f := &stubFunc{answer: "ans", contexts: []string{"c1", "c2"}}
	r := &Runner{Metrics: []Metric{m1, m2}, Func: f.call}
	cases := []EvalCase{
		{ID: "a", Query: "q1", GroundTruth: "gt1"},
		{ID: "b", Query: "q2", GroundTruth: "gt2"},
	}
	rep, err := r.Run(context.Background(), "ds", cases)
	require.NoError(t, err)
	require.NotNil(t, rep)
	assert.Equal(t, "ds", rep.DatasetName)
	assert.Equal(t, 2, rep.SampleCount)
	require.Len(t, rep.Results, 2)
	// Each metric called once per case.
	assert.Equal(t, 2, m1.Calls())
	assert.Equal(t, 2, m2.Calls())
	// Mean scores: m1 = 1.0, m2 = 0.5.
	assert.InDelta(t, 1.0, rep.MeanScores["m1"], 1e-9)
	assert.InDelta(t, 0.5, rep.MeanScores["m2"], 1e-9)
	// Result fields propagated from the case + function under test.
	assert.Equal(t, "ans", rep.Results[0].Answer)
	assert.Equal(t, []string{"c1", "c2"}, rep.Results[0].Contexts)
	assert.Equal(t, "gt1", rep.Results[0].GroundTruth)
}

func TestRunner_PropagatesFuncError(t *testing.T) {
	t.Parallel()
	m := &stubMetric{name: "m", FixedScore: 1}
	f := &stubFunc{err: errors.New("boom")}
	r := &Runner{Metrics: []Metric{m}, Func: f.call}
	rep, err := r.Run(context.Background(), "ds", []EvalCase{{Query: "q"}})
	require.NoError(t, err)
	require.Len(t, rep.Results, 1)
	assert.Empty(t, rep.Results[0].Scores)
	assert.Contains(t, rep.Results[0].Errors["_func"], "boom")
	// Metric must NOT be called when the function under test fails.
	assert.Equal(t, 0, m.Calls())
}

func TestRunner_PropagatesMetricError(t *testing.T) {
	t.Parallel()
	m := &stubMetric{name: "m", Fail: true}
	f := &stubFunc{answer: "a"}
	r := &Runner{Metrics: []Metric{m}, Func: f.call}
	rep, err := r.Run(context.Background(), "ds", []EvalCase{{Query: "q"}})
	require.NoError(t, err)
	require.Len(t, rep.Results, 1)
	assert.InDelta(t, 0.0, rep.Results[0].Scores["m"], 1e-9)
	assert.Contains(t, rep.Results[0].Errors["m"], "metric failed")
	// Mean score for a failed metric is 0.
	assert.InDelta(t, 0.0, rep.MeanScores["m"], 1e-9)
}

func TestRunner_AssignsStableIDs(t *testing.T) {
	t.Parallel()
	m := &stubMetric{name: "m", FixedScore: 1}
	f := &stubFunc{answer: "a"}
	r := &Runner{Metrics: []Metric{m}, Func: f.call}
	cases := []EvalCase{{Query: "q1"}, {Query: "q2"}}
	rep, err := r.Run(context.Background(), "myds", cases)
	require.NoError(t, err)
	assert.Equal(t, "myds-1", rep.Results[0].CaseID)
	assert.Equal(t, "myds-2", rep.Results[1].CaseID)
}

func TestRunner_RespectsConcurrency(t *testing.T) {
	t.Parallel()
	// Concurrency = 1 -> cases run sequentially; verify by counting
	// concurrent in-flight calls.
	var inflight int32
	var maxInflight int32
	f := func(ctx context.Context, q string) (string, []string, error) {
		cur := atomic.AddInt32(&inflight, 1)
		// Track max
		for {
			mx := atomic.LoadInt32(&maxInflight)
			if cur <= mx || atomic.CompareAndSwapInt32(&maxInflight, mx, cur) {
				break
			}
		}
		// Yield to allow another goroutine to enter if concurrency > 1.
		// (Best-effort; we only assert the upper bound.)
		// Then decrement.
		defer atomic.AddInt32(&inflight, -1)
		// Sleep briefly so concurrent goroutines overlap when Concurrency
		// > 1. Without this, each goroutine finishes before the next
		// starts and we cannot assert the semaphore was actually used.
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		return q, nil, nil
	}
	r := &Runner{
		Metrics:    []Metric{&stubMetric{name: "m", FixedScore: 1}},
		Func:       f,
		Concurrency: 4,
	}
	cases := make([]EvalCase, 10)
	for i := range cases {
		cases[i] = EvalCase{Query: "q"}
	}
	_, err := r.Run(context.Background(), "ds", cases)
	require.NoError(t, err)
	// Concurrency cap = 4 so at most 4 in-flight. Allow == since timing
	// may produce exactly the cap.
	assert.LessOrEqual(t, atomic.LoadInt32(&maxInflight), int32(4))
	// With 10 cases and concurrency 4, we should have overlapped at
	// least once (maxInflight >= 2) — otherwise concurrency=1 was
	// effectively used.
	assert.GreaterOrEqual(t, atomic.LoadInt32(&maxInflight), int32(2),
		"expected concurrent execution, maxInflight=%d", atomic.LoadInt32(&maxInflight))
}

func TestNewRunIDIsUnique(t *testing.T) {
	t.Parallel()
	ids := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		id := NewRunID()
		require.NotEmpty(t, id)
		_, dup := ids[id]
		require.False(t, dup, "duplicate run id: %s", id)
		ids[id] = struct{}{}
	}
}
