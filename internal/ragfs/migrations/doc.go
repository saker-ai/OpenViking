// Package migrations embeds ragfs SQL migration files.
//
// Each file is a single forward migration named NNN_<slug>.sql (e.g.
// 001_initial.sql, 002_add_tags.sql). The migrate package lists and
// applies them in lexicographic order, tracking applied versions in a
// `_migrations` table inside the ragfs metadata store.
package migrations

import "embed"

// FS is the embedded filesystem containing every *.sql migration file
// in this directory. Read entries via fs.ReadDir(migrations.FS, ".").
//
//go:embed *.sql
var FS embed.FS
