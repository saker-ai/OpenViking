// Package migrations embeds eval SQL migration files.
//
// Each file is a single forward migration named NNN_<slug>.sql (e.g.
// 001_eval_runs.sql). The eval recorder applies them in lexicographic
// order on construction; re-running on an existing database is a no-op
// because every statement is idempotent (CREATE TABLE IF NOT EXISTS).
package migrations

import "embed"

// FS is the embedded filesystem containing every *.sql migration file
// in this directory.
//
//go:embed *.sql
var FS embed.FS
