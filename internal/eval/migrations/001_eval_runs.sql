-- 001_eval_runs.sql: baseline schema for the eval results recorder.
--
-- The recorder persists one row per eval run plus one row per case
-- result. Mean scores are stored as a JSON blob so the schema does not
-- need to migrate when new metrics are added. All queries against this
-- schema use parameterized placeholders (modernc.org/sqlite).

CREATE TABLE IF NOT EXISTS eval_runs (
    id            TEXT PRIMARY KEY,
    dataset_name  TEXT NOT NULL,
    started_at    TEXT NOT NULL,
    completed_at  TEXT NOT NULL,
    sample_count  INTEGER NOT NULL DEFAULT 0,
    mean_scores   TEXT NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS eval_case_results (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id        TEXT NOT NULL,
    case_id       TEXT NOT NULL,
    query         TEXT NOT NULL,
    answer        TEXT NOT NULL DEFAULT '',
    ground_truth  TEXT NOT NULL DEFAULT '',
    contexts      TEXT NOT NULL DEFAULT '[]',
    scores        TEXT NOT NULL DEFAULT '{}',
    errors        TEXT NOT NULL DEFAULT '{}',
    FOREIGN KEY (run_id) REFERENCES eval_runs(id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS eval_case_results_run_id_idx
    ON eval_case_results(run_id);
