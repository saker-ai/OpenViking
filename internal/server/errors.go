package server

import "errors"

var errNilConfig = errors.New("server: nil config")

// defaultRateLimitRPS / defaultRateLimitBurst are the conservative defaults
// used by BuildApp when no explicit rate-limit config is provided. Per-account
// token bucket; high enough that normal request loads are unaffected but low
// enough that a runaway client cannot exhaust the server.
const (
	defaultRateLimitRPS   = 100.0
	defaultRateLimitBurst = 200
)
