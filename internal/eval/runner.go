package eval

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/rs/xid"
)

// FuncUnderTest is the function the runner exercises for each case. It
// is the RAG pipeline under evaluation: given a query, return the
// generated answer and the retrieved contexts.
//
// The runner is agnostic to how the function produces its outputs —
// it could call an OpenViking server, an in-process retrieve+VLM
// pipeline, or a stub. Tests inject a function that returns canned
// answers + contexts to keep the runner unit tests network-free.
type FuncUnderTest func(ctx context.Context, query string) (answer string, contexts []string, err error)

// Runner runs a list of metrics against a list of cases. Cases are
// executed concurrently bounded by Concurrency; metrics within a case
// run sequentially (they share a grader and would otherwise hammer the
// LLM).
type Runner struct {
	// Metrics is the list of metrics to compute per case. Must be
	// non-empty.
	Metrics []Metric
	// Func is the function under test. Required.
	Func FuncUnderTest
	// Concurrency caps the number of cases evaluated in parallel.
	// Zero defaults to 8. Negative disables concurrency (run
	// sequentially).
	Concurrency int
}

// Run executes the evaluation and returns the aggregate report. The
// dataset name is propagated to EvalReport.DatasetName so recorders
// can persist it.
//
// On a per-case error from FuncUnderTest, the case's Scores map is
// empty and the error string is recorded in Errors under the special
// key "_func". On a per-metric error, the metric's score is 0 and its
// error is recorded in Errors under the metric name. The run itself
// only returns an error when the dataset is empty or the runner is
// misconfigured.
func (r *Runner) Run(ctx context.Context, datasetName string, cases []EvalCase) (*EvalReport, error) {
	if err := r.validate(); err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("eval: empty dataset")
	}
	conc := r.Concurrency
	if conc == 0 {
		conc = 8
	}
	if conc < 0 {
		conc = 1
	}
	if conc > len(cases) {
		conc = len(cases)
	}

	started := time.Now().UTC()
	results := make([]EvalResult, len(cases))

	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, c := range cases {
		idx := i
		cc := c
		// Assign a stable ID when the loader did not.
		if cc.ID == "" {
			cc.ID = fmt.Sprintf("%s-%d", datasetName, idx+1)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[idx] = r.runCase(ctx, cc)
		}()
	}
	wg.Wait()

	completed := time.Now().UTC()
	mean := computeMeanScores(results, r.Metrics)
	return &EvalReport{
		DatasetName: datasetName,
		StartedAt:   started,
		CompletedAt: completed,
		SampleCount: len(results),
		MeanScores:  mean,
		Results:     results,
	}, nil
}

func (r *Runner) validate() error {
	if len(r.Metrics) == 0 {
		return fmt.Errorf("eval: no metrics configured")
	}
	if r.Func == nil {
		return fmt.Errorf("eval: no function under test configured")
	}
	for _, m := range r.Metrics {
		if m == nil {
			return fmt.Errorf("eval: nil metric in list")
		}
	}
	return nil
}

// runCase evaluates one case end-to-end: call the function under test,
// then run every metric, recording errors per-metric.
func (r *Runner) runCase(ctx context.Context, c EvalCase) EvalResult {
	res := EvalResult{
		CaseID:      c.ID,
		Query:       c.Query,
		GroundTruth: c.GroundTruth,
		Scores:      map[string]float64{},
		Errors:      map[string]string{},
	}
	answer, contexts, err := r.Func(ctx, c.Query)
	if err != nil {
		res.Errors["_func"] = err.Error()
		// Still run metrics that don't need an answer? No — the
		// function-under-test error means the RAG pipeline failed; we
		// keep the case in the report but skip metric evaluation so
		// the error is clearly attributable.
		return res
	}
	res.Answer = answer
	res.Contexts = contexts
	for _, m := range r.Metrics {
		score, err := m.Score(ctx, c, answer, contexts)
		res.Scores[m.Name()] = score
		if err != nil {
			res.Errors[m.Name()] = err.Error()
		}
	}
	return res
}

// computeMeanScores returns the per-metric mean across all cases that
// have a non-error score. Cases with an error for a metric contribute
// 0 to that metric's mean (matching the Python behavior where a failed
// metric is recorded as 0 rather than dropped).
func computeMeanScores(results []EvalResult, metrics []Metric) map[string]float64 {
	means := make(map[string]float64, len(metrics))
	for _, m := range metrics {
		var sum float64
		var n int
		for _, r := range results {
			if score, ok := r.Scores[m.Name()]; ok {
				sum += score
				n++
			}
		}
		if n > 0 {
			means[m.Name()] = sum / float64(n)
		} else {
			means[m.Name()] = 0
		}
	}
	return means
}

// NewRunID returns a globally-unique run identifier. Used by the
// recorder when persisting a report. Exposed so callers can correlate
// runs across logs and database rows.
func NewRunID() string {
	return xid.New().String()
}
