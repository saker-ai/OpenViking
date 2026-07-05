// Package middleware — circuit breaker thin wrapper around
// github.com/sony/gobreaker/v2. The wrapper preserves the prior
// hand-rolled API (CircuitConfig / CircuitBreaker / Execute[T]) so
// existing callers do not need to change, while delegating state
// management to the mature library.
package middleware

import (
	"context"
	"errors"
	"time"

	gobreaker "github.com/sony/gobreaker/v2"
)

// CircuitState reports the current state of a breaker.
type CircuitState int

const (
	// StateClosed means requests flow through; failures are counted.
	StateClosed CircuitState = iota
	// StateOpen means requests are short-circuited; the cooldown is in effect.
	StateOpen
	// StateHalfOpen means a single probe request is allowed through to test
	// the downstream service.
	StateHalfOpen
)

// String returns the lower-case name of the state.
func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// CircuitConfig tunes a CircuitBreaker.
type CircuitConfig struct {
	// Name is a human-readable label used in errors and logs.
	Name string
	// MaxConsecutiveFailures is the number of consecutive failures that
	// trips the breaker from Closed to Open.
	MaxConsecutiveFailures int
	// ErrorRateThreshold is the failure ratio (0.0-1.0) over the rolling
	// window that trips the breaker. Set to 0 to disable.
	ErrorRateThreshold float64
	// MinRequestsForRate is the minimum sample size before ErrorRateThreshold
	// is evaluated. Below this count only consecutive failures apply.
	MinRequestsForRate int
	// Cooldown is how long the breaker stays Open before transitioning
	// to HalfOpen.
	Cooldown time.Duration
	// HalfOpenMaxProbe is the number of successful probes in HalfOpen that
	// closes the breaker. A single failure in HalfOpen re-opens it.
	HalfOpenMaxProbe int
	// OnStateChange is invoked after a transition. Optional.
	OnStateChange func(name string, from, to CircuitState)
}

// CircuitBreaker wraps outbound calls (LLM/embedder/vectordb) with a
// failure-tripping breaker. It is intended for use inside service adapters,
// not as inbound HTTP middleware.
//
// The implementation delegates to github.com/sony/gobreaker/v2. We hold a
// single *gobreaker.CircuitBreaker[any] regardless of the type parameter
// the caller passes to Execute[T]; the breaker state is independent of the
// value type, so per-type breakers would fragment state needlessly.
type CircuitBreaker struct {
	cfg CircuitConfig
	cb  *gobreaker.CircuitBreaker[any]
}

// NewCircuitBreaker constructs a breaker from cfg. Defaults are applied
// when zero-valued fields are encountered to avoid unusable state.
func NewCircuitBreaker(cfg CircuitConfig) *CircuitBreaker {
	if cfg.MaxConsecutiveFailures <= 0 {
		cfg.MaxConsecutiveFailures = 5
	}
	if cfg.Cooldown <= 0 {
		cfg.Cooldown = 30 * time.Second
	}
	if cfg.HalfOpenMaxProbe <= 0 {
		cfg.HalfOpenMaxProbe = 1
	}
	if cfg.MinRequestsForRate <= 0 {
		cfg.MinRequestsForRate = 10
	}
	maxConsec := uint32(cfg.MaxConsecutiveFailures)
	rateThreshold := cfg.ErrorRateThreshold
	minReq := uint32(cfg.MinRequestsForRate)

	settings := gobreaker.Settings{
		Name:        cfg.Name,
		MaxRequests: uint32(cfg.HalfOpenMaxProbe),
		Interval:    0, // no rolling-window reset; counts accrue for the current generation
		Timeout:     cfg.Cooldown,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			if maxConsec > 0 && c.ConsecutiveFailures >= maxConsec {
				return true
			}
			if rateThreshold > 0 && c.Requests >= minReq {
				if float64(c.TotalFailures)/float64(c.Requests) >= rateThreshold {
					return true
				}
			}
			return false
		},
		OnStateChange: nil, // set below if callback provided
	}
	if cfg.OnStateChange != nil {
		userCB := cfg.OnStateChange
		settings.OnStateChange = func(name string, from, to gobreaker.State) {
			userCB(name, toCircuitState(from), toCircuitState(to))
		}
	}
	return &CircuitBreaker{cfg: cfg, cb: gobreaker.NewCircuitBreaker[any](settings)}
}

// toCircuitState maps a gobreaker.State to the local CircuitState enum.
func toCircuitState(s gobreaker.State) CircuitState {
	switch s {
	case gobreaker.StateClosed:
		return StateClosed
	case gobreaker.StateHalfOpen:
		return StateHalfOpen
	case gobreaker.StateOpen:
		return StateOpen
	default:
		return StateClosed
	}
}

// State returns the current breaker state. Safe for concurrent use.
func (b *CircuitBreaker) State() CircuitState {
	if b == nil {
		return StateClosed
	}
	return toCircuitState(b.cb.State())
}

// ErrCircuitOpen is returned by Execute when the breaker is Open.
var ErrCircuitOpen = errors.New("circuit breaker open")

// ErrTooManyProbes is returned when HalfOpen already has an in-flight probe.
var ErrTooManyProbes = errors.New("circuit breaker half-open: probe in flight")

// Execute runs fn under the breaker's protection. It returns the same
// value/error fn returns, or ErrCircuitOpen when the breaker is Open.
func Execute[T any](b *CircuitBreaker, ctx context.Context, fn func(ctx context.Context) (T, error)) (T, error) {
	var zero T
	if b == nil {
		return fn(ctx)
	}
	v, err := b.cb.Execute(func() (any, error) {
		return fn(ctx)
	})
	if err == nil {
		if v == nil {
			return zero, nil
		}
		t, ok := v.(T)
		if !ok {
			return zero, ErrCircuitOpen
		}
		return t, nil
	}
	// Translate gobreaker sentinel errors to the local equivalents so
	// callers can errors.Is against ErrCircuitOpen / ErrTooManyProbes.
	switch {
	case errors.Is(err, gobreaker.ErrOpenState):
		return zero, ErrCircuitOpen
	case errors.Is(err, gobreaker.ErrTooManyRequests):
		return zero, ErrTooManyProbes
	default:
		// fn returned an error — pass it through as-is so callers see
		// the original downstream error, not a wrapped variant.
		if v != nil {
			if t, ok := v.(T); ok {
				return t, err
			}
		}
		return zero, err
	}
}
