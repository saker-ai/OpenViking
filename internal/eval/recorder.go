package eval

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver; no cgo

	"github.com/saker-ai/ctxhub/internal/eval/migrations"
)

// Recorder persists eval runs to SQLite. One row per run in
// `eval_runs`; one row per case result in `eval_case_results`. Mean
// scores and per-case scores are JSON blobs so the schema does not
// need to migrate when new metrics are added.
//
// The recorder is safe for concurrent use. A Go-level mutex
// serializes access from the owning process; SQLite's _txlock=immediate
// plus busy_timeout=5000 serializes cross-process writers.
//
// path may be ":memory:" for an in-memory database (useful for tests)
// or a filesystem path for a durable store. The schema is created
// idempotently on construction via the embedded migrations.
type Recorder struct {
	mu sync.Mutex
	db *sql.DB
}

// NewRecorder opens (or creates) the SQLite database at path and runs
// migrations. path may be ":memory:" for an in-memory database.
func NewRecorder(path string) (*Recorder, error) {
	if path == "" {
		return nil, fmt.Errorf("eval: recorder requires a non-empty path")
	}
	dsn := normalizeSQLiteDSN(path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("eval: open sqlite %s: %w", path, err)
	}
	// SQLite is single-writer; serialize the pool so :memory: (without
	// cache=shared) and file-backed stores behave identically under
	// concurrent access from the same process.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("eval: sqlite pragmas: %w", err)
	}
	r := &Recorder{db: db}
	if err := r.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("eval: recorder migrate: %w", err)
	}
	return r, nil
}

// memCounter assigns a unique identifier to each ":memory:" request so
// concurrent recorders do not share the same in-memory database (which
// would happen with a fixed "file::memory:?cache=shared" DSN).
var memCounter int64

// normalizeSQLiteDSN rewrites ":memory:" into a unique shared-cache
// file DSN and appends _txlock=immediate so all BEGIN transactions
// acquire a write lock immediately (avoids "database is locked" under
// concurrent writers). Mirrors internal/queuefs/semantic.go but with a
// per-call counter so parallel tests using ":memory:" do not collide.
func normalizeSQLiteDSN(path string) string {
	if path == ":memory:" {
		n := atomic.AddInt64(&memCounter, 1)
		return fmt.Sprintf("file:evalmem_%d?mode=memory&cache=shared&_txlock=immediate", n)
	}
	if !strings.Contains(path, "?") {
		return path + "?_txlock=immediate"
	}
	if !strings.Contains(path, "_txlock=") {
		return path + "&_txlock=immediate"
	}
	return path
}

// Close releases the underlying SQLite handle. Idempotent.
func (r *Recorder) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.db == nil {
		return nil
	}
	err := r.db.Close()
	r.db = nil
	return err
}

// migrate applies all embedded SQL migrations in lexicographic order.
// Each migration is idempotent (CREATE TABLE IF NOT EXISTS), so
// re-running on an existing database is safe.
func (r *Recorder) migrate() error {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := fs.ReadFile(migrations.FS, f)
		if err != nil {
			return fmt.Errorf("read %s: %w", f, err)
		}
		if _, err := r.db.Exec(string(b)); err != nil {
			return fmt.Errorf("apply %s: %w", f, err)
		}
	}
	return nil
}

// SaveRun persists a report and all its per-case results. The run ID
// is assigned by the caller (use NewRunID() for a fresh xid); on
// conflict the existing run is overwritten (INSERT OR REPLACE) so
// re-running with the same ID updates the row.
//
// All inserts run inside a single transaction so a partial write is
// never visible.
func (r *Recorder) SaveRun(ctx context.Context, runID string, report *EvalReport) error {
	if runID == "" {
		return fmt.Errorf("eval: empty run id")
	}
	if report == nil {
		return fmt.Errorf("eval: nil report")
	}
	meanJSON, err := json.Marshal(report.MeanScores)
	if err != nil {
		return fmt.Errorf("eval: marshal mean scores: %w", err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("eval: save begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `
INSERT INTO eval_runs (id, dataset_name, started_at, completed_at, sample_count, mean_scores)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    dataset_name  = excluded.dataset_name,
    started_at    = excluded.started_at,
    completed_at  = excluded.completed_at,
    sample_count  = excluded.sample_count,
    mean_scores   = excluded.mean_scores`,
		runID, report.DatasetName,
		report.StartedAt.UTC().Format(time.RFC3339Nano),
		report.CompletedAt.UTC().Format(time.RFC3339Nano),
		report.SampleCount, string(meanJSON),
	); err != nil {
		return fmt.Errorf("eval: save run: %w", err)
	}
	// Replace case results for this run: delete then insert. Cheaper
	// than per-row ON CONFLICT and the table is small per run.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM eval_case_results WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("eval: clear case results: %w", err)
	}
	for _, res := range report.Results {
		if err := saveCaseResult(ctx, tx, runID, res); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("eval: save commit: %w", err)
	}
	return nil
}

func saveCaseResult(ctx context.Context, tx *sql.Tx, runID string, res EvalResult) error {
	contextsJSON, err := json.Marshal(res.Contexts)
	if err != nil {
		return fmt.Errorf("eval: marshal contexts: %w", err)
	}
	scoresJSON, err := json.Marshal(res.Scores)
	if err != nil {
		return fmt.Errorf("eval: marshal scores: %w", err)
	}
	errorsJSON, err := json.Marshal(res.Errors)
	if err != nil {
		return fmt.Errorf("eval: marshal errors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
INSERT INTO eval_case_results
    (run_id, case_id, query, answer, ground_truth, contexts, scores, errors)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, res.CaseID, res.Query, res.Answer, res.GroundTruth,
		string(contextsJSON), string(scoresJSON), string(errorsJSON),
	); err != nil {
		return fmt.Errorf("eval: save case result: %w", err)
	}
	return nil
}

// RecordedRun is one row of eval_runs joined with its case results.
type RecordedRun struct {
	ID          string
	DatasetName string
	StartedAt   time.Time
	CompletedAt time.Time
	SampleCount int
	MeanScores  map[string]float64
	Results     []EvalResult
}

// GetRun loads a run and all its case results. Returns (nil, nil) when
// the run ID does not exist (matching the queuefs store convention).
func (r *Recorder) GetRun(ctx context.Context, runID string) (*RecordedRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var (
		datasetName  string
		startedAt    string
		completedAt  string
		sampleCount  int
		meanJSON     string
	)
	err := r.db.QueryRowContext(ctx, `
SELECT id, dataset_name, started_at, completed_at, sample_count, mean_scores
FROM eval_runs WHERE id = ?`, runID).
		Scan(&runID, &datasetName, &startedAt, &completedAt, &sampleCount, &meanJSON)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("eval: get run: %w", err)
	}
	out := &RecordedRun{
		ID:          runID,
		DatasetName: datasetName,
		SampleCount: sampleCount,
		MeanScores:  map[string]float64{},
	}
	out.StartedAt, _ = time.Parse(time.RFC3339Nano, startedAt)
	out.CompletedAt, _ = time.Parse(time.RFC3339Nano, completedAt)
	if meanJSON != "" {
		_ = json.Unmarshal([]byte(meanJSON), &out.MeanScores)
	}
	rows, err := r.db.QueryContext(ctx, `
SELECT case_id, query, answer, ground_truth, contexts, scores, errors
FROM eval_case_results WHERE run_id = ? ORDER BY id ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("eval: get case results: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			caseID       string
			query        string
			answer       string
			groundTruth  string
			contextsJSON string
			scoresJSON   string
			errorsJSON   string
		)
		if err := rows.Scan(&caseID, &query, &answer, &groundTruth, &contextsJSON, &scoresJSON, &errorsJSON); err != nil {
			return nil, fmt.Errorf("eval: scan case result: %w", err)
		}
		res := EvalResult{
			CaseID:      caseID,
			Query:       query,
			Answer:      answer,
			GroundTruth: groundTruth,
			Scores:      map[string]float64{},
			Errors:      map[string]string{},
		}
		if contextsJSON != "" {
			_ = json.Unmarshal([]byte(contextsJSON), &res.Contexts)
		}
		if scoresJSON != "" {
			_ = json.Unmarshal([]byte(scoresJSON), &res.Scores)
		}
		if errorsJSON != "" {
			_ = json.Unmarshal([]byte(errorsJSON), &res.Errors)
		}
		out.Results = append(out.Results, res)
	}
	return out, rows.Err()
}

// ListRuns returns run metadata (no case results) ordered by
// started_at DESC. Pass 0 for limit to list all runs.
func (r *Recorder) ListRuns(ctx context.Context, limit int) ([]RecordedRun, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var (
		rows *sql.Rows
		err  error
	)
	if limit > 0 {
		rows, err = r.db.QueryContext(ctx, `
SELECT id, dataset_name, started_at, completed_at, sample_count, mean_scores
FROM eval_runs ORDER BY started_at DESC LIMIT ?`, limit)
	} else {
		rows, err = r.db.QueryContext(ctx, `
SELECT id, dataset_name, started_at, completed_at, sample_count, mean_scores
FROM eval_runs ORDER BY started_at DESC`)
	}
	if err != nil {
		return nil, fmt.Errorf("eval: list runs: %w", err)
	}
	defer rows.Close()
	var out []RecordedRun
	for rows.Next() {
		var (
			id           string
			datasetName  string
			startedAt    string
			completedAt  string
			sampleCount  int
			meanJSON     string
		)
		if err := rows.Scan(&id, &datasetName, &startedAt, &completedAt, &sampleCount, &meanJSON); err != nil {
			return nil, fmt.Errorf("eval: scan run: %w", err)
		}
		run := RecordedRun{
			ID:          id,
			DatasetName: datasetName,
			SampleCount: sampleCount,
			MeanScores:  map[string]float64{},
		}
		run.StartedAt, _ = time.Parse(time.RFC3339Nano, startedAt)
		run.CompletedAt, _ = time.Parse(time.RFC3339Nano, completedAt)
		if meanJSON != "" {
			_ = json.Unmarshal([]byte(meanJSON), &run.MeanScores)
		}
		out = append(out, run)
	}
	return out, rows.Err()
}
