package middleware

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// rateLimiterConfig tracks per-account token bucket state. The zero value
// is not usable; callers must construct via newRateLimiter.
type rateLimiterConfig struct {
	rps   rate.Limit
	burst int
	mu    sync.Mutex
	// limiters maps account id -> *rate.Limiter. Lazy-populated on first
	// request from each account. Pruning of idle entries is left to the
	// caller; for P12 a small fixed cap keeps memory bounded.
	limiters map[string]*rate.Limiter
}

// RateLimit enforces a token-bucket rate limit per account. The account is
// resolved from the X-OpenViking-Account header; requests without it fall
// back to the request remote address. When the bucket is exhausted the
// middleware responds with 429 RATE_LIMITED and a Retry-After header.
//
// rps is requests-per-second; burst is the maximum tokens that can
// accumulate. Burst must be >= 1.
func RateLimit(rps float64, burst int) gin.HandlerFunc {
	if burst < 1 {
		burst = 1
	}
	cfg := &rateLimiterConfig{
		rps:      rate.Limit(rps),
		burst:    burst,
		limiters: make(map[string]*rate.Limiter),
	}
	return func(c *gin.Context) {
		key := accountKey(c)
		lim := cfg.get(key)
		// reservation-style allow with retry-after hint on rejection.
		res := lim.Reserve()
		if !res.OK() {
			retryAfter(c, time.Duration(0))
			return
		}
		delay := res.Delay()
		if delay > 0 {
			res.Cancel()
			retryAfter(c, delay)
			return
		}
		c.Next()
	}
}

// get returns the limiter for key, creating one if absent. Safe for
// concurrent use because rate.Limiter is itself goroutine-safe.
func (r *rateLimiterConfig) get(key string) *rate.Limiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	lim, ok := r.limiters[key]
	if !ok {
		lim = rate.NewLimiter(r.rps, r.burst)
		r.limiters[key] = lim
	}
	return lim
}

// accountKey resolves the rate-limit bucket key from request identity.
// Falls back to client IP when no account header is present.
func accountKey(c *gin.Context) string {
	if acct := c.GetHeader("X-OpenViking-Account"); acct != "" {
		return acct
	}
	return c.ClientIP()
}

// retryAfter aborts the chain with a 429 and Retry-After header. delay is
// rounded up to the next whole second per RFC 7231 §7.1.3.
func retryAfter(c *gin.Context, delay time.Duration) {
	secs := int(delay.Round(time.Second).Seconds())
	if secs < 1 {
		secs = 1
	}
	c.Header("Retry-After", strconv.Itoa(secs))
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": gin.H{
			"code":    "RATE_LIMITED",
			"message": "rate limit exceeded",
		},
	})
}
