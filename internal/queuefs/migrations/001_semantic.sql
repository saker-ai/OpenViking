-- 001_semantic.sql: queuefs semantic layer schema.
--
-- Implements the queuefs semantic layer (priority queue + lease/claim/
-- renew/complete/abandon + DLQ + per-account isolation + delayed tasks)
-- backed by SQLite. The schema is created idempotently; the SemanticStore
-- applies this file via embed.FS on construction.
--
-- Concurrency: SQLite serializes writes via BEGIN IMMEDIATE (_txlock=immediate
-- in the DSN) plus a Go-level mutex in SemanticStore. At most one worker
-- holds a lease on a given task at any time.

CREATE TABLE IF NOT EXISTS queuefs_tasks (
    id            TEXT PRIMARY KEY,
    account_id    TEXT NOT NULL,
    type          TEXT NOT NULL,
    payload       BLOB,
    priority      INTEGER NOT NULL,
    max_retries   INTEGER NOT NULL DEFAULT 0,
    attempts      INTEGER NOT NULL DEFAULT 0,
    delay_until   TEXT NOT NULL DEFAULT '',   -- RFC3339Nano; '' = immediately visible
    enqueued_at   TEXT NOT NULL,              -- RFC3339Nano; FIFO within priority
    status        TEXT NOT NULL DEFAULT 'pending', -- 'pending' | 'leased'
    lease_id      TEXT,
    lease_expires TEXT,                       -- RFC3339Nano
    last_error    TEXT
);

-- Claim index: highest priority first, oldest first, scoped to account+status.
CREATE INDEX IF NOT EXISTS queuefs_tasks_claim
    ON queuefs_tasks(account_id, status, priority DESC, enqueued_at);

-- Renew / Complete / Abandon look up by lease_id.
CREATE INDEX IF NOT EXISTS queuefs_tasks_lease
    ON queuefs_tasks(lease_id);

-- RecoverStale scans by status + lease_expires.
CREATE INDEX IF NOT EXISTS queuefs_tasks_stale
    ON queuefs_tasks(status, lease_expires);

-- Dead-letter queue: tasks that exhausted MaxRetries.
CREATE TABLE IF NOT EXISTS queuefs_dead_letter (
    id           TEXT PRIMARY KEY,
    account_id   TEXT NOT NULL,
    type         TEXT NOT NULL,
    payload      BLOB,
    priority     INTEGER NOT NULL,
    attempts     INTEGER NOT NULL,
    last_error   TEXT,
    dead_at      TEXT NOT NULL                -- RFC3339Nano
);

CREATE INDEX IF NOT EXISTS queuefs_dl_account
    ON queuefs_dead_letter(account_id, dead_at);
