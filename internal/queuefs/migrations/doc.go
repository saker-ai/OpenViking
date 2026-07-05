// Package migrations embeds queuefs SQL migration files.
//
// Each file is a single forward migration named NNN_<slug>.sql (e.g.
// 001_semantic.sql). The SemanticStore.migrate method lists and applies
// them in lexicographic order on construction. Migrations are idempotent
// (use CREATE TABLE IF NOT EXISTS) so re-running on an existing database
// is safe.
package migrations

import "embed"

// FS is the embedded filesystem containing every *.sql migration file
// in this directory.
//
//go:embed *.sql
var FS embed.FS
