// Package migrations embeds the OAuth provider-token SQL schema.
//
// The single file 001_oauth_tokens.sql creates the oauth_provider_tokens
// and oauth_state_cache tables. The store applies it on startup via
// database/sql Exec (idempotent CREATE TABLE IF NOT EXISTS).
package migrations

import "embed"

// FS is the embedded filesystem containing every *.sql migration file
// in this directory.
//
//go:embed *.sql
var FS embed.FS
