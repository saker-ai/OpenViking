// Package eval implements the OpenViking evaluation framework — a Go
// port of the Python `openviking.eval.ragas` surface (design §9.5.1).
//
// The package exposes:
//   - EvalCase / EvalResult / EvalReport: data types mirrored from
//     openviking/eval/ragas/types.py.
//   - Metric interface + 5 implementations: Faithfulness,
//     AnswerRelevancy, ContextualPrecision, ContextualRecall,
//     ContextualRelevance. Each metric uses an LLM (via
//     internal/models/vlm) for grading, the same pattern as Python
//     ragas.
//   - Runner: runs a function under test against a dataset, scoring
//     every case with every metric concurrently.
//   - Recorder: persists eval runs to SQLite via embedded migrations.
//   - LoadDataset: loads JSONL datasets compatible with the Python
//     openviking/eval/datasets format.
//
// The package does NOT depend on github.com/exyre/ragas or any
// Python-specific library; the metric logic is ported to Go. All LLM
// calls go through internal/models/vlm so tests inject a vlm.Stub and
// never hit the network.
package eval

import "time"

// EvalCase is a single evaluation sample. It mirrors EvalSample from
// openviking/eval/ragas/types.py, renamed to "Case" to match the Go
// convention of short type names and to avoid stutter with the package
// name (eval.EvalSample -> eval.EvalCase).
type EvalCase struct {
	// ID is an optional stable identifier. When empty the loader
	// assigns the 1-based index of the case in the dataset.
	ID string `json:"id"`
	// Query is the input question (Python: query).
	Query string `json:"query"`
	// GroundTruth is the reference answer (Python: ground_truth).
	// Optional; metrics that need it (ContextualRecall) return an
	// error when it is empty.
	GroundTruth string `json:"ground_truth,omitempty"`
	// Meta carries arbitrary per-case metadata (Python: meta). Keys
	// are strings to keep the JSON round-trip stable across providers.
	Meta map[string]string `json:"meta,omitempty"`
}

// EvalResult is the result of evaluating one case. It mirrors
// EvalResult from openviking/eval/ragas/types.py but flattens the
// sample back into the result (the Python version embeds the full
// EvalSample; the Go version carries only the fields needed for
// reporting).
type EvalResult struct {
	CaseID   string             `json:"case_id"`
	Query    string             `json:"query"`
	Answer   string             `json:"answer"`
	// GroundTruth is copied from the input EvalCase so the recorder
	// can persist it for later analysis without needing a separate
	// join on the dataset file.
	GroundTruth string             `json:"ground_truth"`
	Contexts []string           `json:"contexts"`
	Scores   map[string]float64 `json:"scores"`
	// Errors maps metric name -> error message when a metric failed
	// for this case. The metric's score is 0 in that case. Keeping
	// errors per-metric (rather than failing the whole run) matches
	// the Python behavior where one bad sample does not abort the
	// dataset.
	Errors map[string]string `json:"errors,omitempty"`
}

// EvalReport is the aggregate result of evaluating a dataset. It
// mirrors SummaryResult from openviking/eval/ragas/types.py.
type EvalReport struct {
	DatasetName  string             `json:"dataset_name"`
	StartedAt    time.Time          `json:"started_at"`
	CompletedAt  time.Time          `json:"completed_at"`
	SampleCount  int                `json:"sample_count"`
	MeanScores   map[string]float64 `json:"mean_scores"`
	Results      []EvalResult       `json:"results"`
}
