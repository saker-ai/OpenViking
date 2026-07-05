package eval

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecorder_SaveAndGetRun(t *testing.T) {
	t.Parallel()
	r, err := NewRecorder(":memory:")
	require.NoError(t, err)
	defer r.Close()

	report := &EvalReport{
		DatasetName: "ds",
		StartedAt:   time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC),
		CompletedAt: time.Date(2026, 1, 1, 12, 0, 5, 0, time.UTC),
		SampleCount: 2,
		MeanScores: map[string]float64{
			"faithfulness": 0.75,
			"answer_relevancy": 0.5,
		},
		Results: []EvalResult{
			{
				CaseID:      "ds-1",
				Query:       "q1",
				Answer:      "a1",
				GroundTruth: "gt1",
				Contexts:    []string{"c1", "c2"},
				Scores:      map[string]float64{"faithfulness": 1.0, "answer_relevancy": 0.5},
				Errors:      map[string]string{},
			},
			{
				CaseID:      "ds-2",
				Query:       "q2",
				Answer:      "a2",
				GroundTruth: "gt2",
				Contexts:    []string{"c3"},
				Scores:      map[string]float64{"faithfulness": 0.5, "answer_relevancy": 0.5},
				Errors:      map[string]string{"faithfulness": "metric failed"},
			},
		},
	}
	runID := "run-123"
	require.NoError(t, r.SaveRun(context.Background(), runID, report))

	got, err := r.GetRun(context.Background(), runID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, runID, got.ID)
	assert.Equal(t, "ds", got.DatasetName)
	assert.Equal(t, 2, got.SampleCount)
	assert.InDelta(t, 0.75, got.MeanScores["faithfulness"], 1e-9)
	assert.InDelta(t, 0.5, got.MeanScores["answer_relevancy"], 1e-9)
	require.Len(t, got.Results, 2)
	// Results ordered by id ASC = insertion order.
	assert.Equal(t, "ds-1", got.Results[0].CaseID)
	assert.Equal(t, "a1", got.Results[0].Answer)
	assert.Equal(t, "gt1", got.Results[0].GroundTruth)
	assert.Equal(t, []string{"c1", "c2"}, got.Results[0].Contexts)
	assert.InDelta(t, 1.0, got.Results[0].Scores["faithfulness"], 1e-9)
	assert.Equal(t, "metric failed", got.Results[1].Errors["faithfulness"])
}

func TestRecorder_GetRunMissing(t *testing.T) {
	t.Parallel()
	r, err := NewRecorder(":memory:")
	require.NoError(t, err)
	defer r.Close()
	got, err := r.GetRun(context.Background(), "nope")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestRecorder_SaveRunOverwritesExisting(t *testing.T) {
	t.Parallel()
	r, err := NewRecorder(":memory:")
	require.NoError(t, err)
	defer r.Close()
	ctx := context.Background()
	// First save: 1 case.
	first := &EvalReport{
		DatasetName: "ds",
		StartedAt:   time.Now().UTC(),
		CompletedAt: time.Now().UTC(),
		SampleCount: 1,
		MeanScores:  map[string]float64{"faithfulness": 0.5},
		Results: []EvalResult{
			{CaseID: "c1", Query: "q1", Answer: "a1", Scores: map[string]float64{"faithfulness": 0.5}},
		},
	}
	require.NoError(t, r.SaveRun(ctx, "rid", first))
	// Second save with same ID: 2 cases. Should replace the first.
	second := &EvalReport{
		DatasetName: "ds-v2",
		StartedAt:   time.Now().UTC(),
		CompletedAt: time.Now().UTC(),
		SampleCount: 2,
		MeanScores:  map[string]float64{"faithfulness": 0.9},
		Results: []EvalResult{
			{CaseID: "c1", Query: "q1", Answer: "a1", Scores: map[string]float64{"faithfulness": 0.9}},
			{CaseID: "c2", Query: "q2", Answer: "a2", Scores: map[string]float64{"faithfulness": 0.9}},
		},
	}
	require.NoError(t, r.SaveRun(ctx, "rid", second))
	got, err := r.GetRun(ctx, "rid")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "ds-v2", got.DatasetName)
	assert.Equal(t, 2, got.SampleCount)
	require.Len(t, got.Results, 2)
	assert.InDelta(t, 0.9, got.MeanScores["faithfulness"], 1e-9)
}

func TestRecorder_ListRuns(t *testing.T) {
	t.Parallel()
	r, err := NewRecorder(":memory:")
	require.NoError(t, err)
	defer r.Close()
	ctx := context.Background()
	for i, name := range []string{"a", "b", "c"} {
		rep := &EvalReport{
			DatasetName: name,
			StartedAt:   time.Date(2026, 1, 1, 12, 0, i, 0, time.UTC),
			CompletedAt: time.Date(2026, 1, 1, 12, 0, i+1, 0, time.UTC),
			SampleCount: i + 1,
			MeanScores:  map[string]float64{"faithfulness": float64(i) / 3.0},
			Results: []EvalResult{
				{CaseID: "c", Query: "q", Answer: "a", Scores: map[string]float64{"faithfulness": 1.0}},
			},
		}
		require.NoError(t, r.SaveRun(ctx, "run-"+name, rep))
	}
	runs, err := r.ListRuns(ctx, 0)
	require.NoError(t, err)
	require.Len(t, runs, 3)
	// DESC by started_at -> "c" first.
	assert.Equal(t, "c", runs[0].DatasetName)
	assert.Equal(t, "a", runs[2].DatasetName)

	limited, err := r.ListRuns(ctx, 2)
	require.NoError(t, err)
	require.Len(t, limited, 2)
	assert.Equal(t, "c", limited[0].DatasetName)
}

func TestRecorder_SaveRunRejectsBadInput(t *testing.T) {
	t.Parallel()
	r, err := NewRecorder(":memory:")
	require.NoError(t, err)
	defer r.Close()
	require.Error(t, r.SaveRun(context.Background(), "", &EvalReport{}))
	require.Error(t, r.SaveRun(context.Background(), "id", nil))
}

func TestRecorder_NormalizeSQLiteDSN(t *testing.T) {
	t.Parallel()
	// ":memory:" maps to a unique shared-cache DSN per call.
	got := normalizeSQLiteDSN(":memory:")
	assert.True(t, strings.HasPrefix(got, "file:evalmem_"),
		"unexpected :memory: DSN: %s", got)
	assert.Contains(t, got, "mode=memory")
	assert.Contains(t, got, "cache=shared")
	assert.Contains(t, got, "_txlock=immediate")
	assert.Equal(t, "/tmp/x.db?_txlock=immediate", normalizeSQLiteDSN("/tmp/x.db"))
	assert.Equal(t, "/tmp/x.db?_busy=1&_txlock=immediate", normalizeSQLiteDSN("/tmp/x.db?_busy=1"))
	assert.Equal(t, "/tmp/x.db?_txlock=immediate&_busy=1", normalizeSQLiteDSN("/tmp/x.db?_txlock=immediate&_busy=1"))
}
