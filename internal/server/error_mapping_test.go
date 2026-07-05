// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// error_mapping_test.go is the test suite for error_mapping.go. It
// covers each of the six categories from the Python reference:
//
//  1. Upstream HTTP errors (table-driven over upstreamHTTPStatusToCode)
//  2. AGFS errors (sentinels + substring heuristics)
//  3. Lock errors (acquire/refresh/lease)
//  4. SSE errors (connection drop, malformed event)
//  5. OAuth errors (invalid_grant, expired, revoked)
//  6. Validation errors (missing field, bad format)
//
// Plus the AppError pass-through and the generic 500 fallback.

// httpStatusErr is a test double for the HTTPStatusError interface.
type httpStatusErr struct {
	status int
	inner  error
}

func (e *httpStatusErr) Error() string {
	if e.inner != nil {
		return fmt.Sprintf("http %d: %v", e.status, e.inner)
	}
	return fmt.Sprintf("http %d", e.status)
}

func (e *httpStatusErr) HTTPStatus() int { return e.status }

func (e *httpStatusErr) Unwrap() error { return e.inner }

// TestMapError_NilError confirms that nil falls through to the
// generic 500 INTERNAL_ERROR path.
func TestMapError_NilError(t *testing.T) {
	status, code, msg := MapError(nil)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, domain.CodeInternalError, code)
	assert.Equal(t, "internal error", msg)
}

// TestMapError_UnknownError confirms that an unrecognized error
// falls through to 500 INTERNAL_ERROR with the original message.
func TestMapError_UnknownError(t *testing.T) {
	err := errors.New("something weird happened")
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, domain.CodeInternalError, code)
	assert.Equal(t, "something weird happened", msg)
}

// TestMapError_AppErrorPassThrough confirms that *domain.AppError
// values pass through with their explicit code/status/message.
func TestMapError_AppErrorPassThrough(t *testing.T) {
	err := domain.Wrap(domain.CodeResourceNotFound, 404,
		errors.New("resource gone"))
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, domain.CodeResourceNotFound, code)
	assert.Equal(t, "resource gone", msg)
}

// TestMapError_AppErrorWithoutStatus confirms an AppError with a
// zero Status field defaults to 500.
func TestMapError_AppErrorWithoutStatus(t *testing.T) {
	err := &domain.AppError{Code: domain.CodeInternalError, Err: errors.New("boom")}
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, domain.CodeInternalError, code)
	assert.Equal(t, "boom", msg)
}

// TestMapError_AppErrorWithoutErr confirms an AppError with a nil
// Err field falls back to the code as the message.
func TestMapError_AppErrorWithoutErr(t *testing.T) {
	err := &domain.AppError{Code: domain.CodeConflict, Status: 409}
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, domain.CodeConflict, code)
	assert.Equal(t, domain.CodeConflict, msg)
}

// TestMapError_UpstreamHTTPStatusTable is the table-driven test for
// the upstream HTTP status → error code mapping. Mirrors the Python
// _UPSTREAM_HTTP_STATUS_TO_ERROR_CODE table.
func TestMapError_UpstreamHTTPStatusTable(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		wantCode   string
		wantStatus int
	}{
		{"400 BadRequest", 400, domain.CodeInvalidArgument, http.StatusBadRequest},
		{"401 Unauthorized", 401, domain.CodeUnauthenticated, http.StatusUnauthorized},
		{"402 PaymentRequired", 402, domain.CodeResourceExhausted, http.StatusTooManyRequests},
		{"403 Forbidden", 403, domain.CodePermissionDenied, http.StatusForbidden},
		{"404 NotFound", 404, domain.CodeNotFound, http.StatusNotFound},
		{"408 RequestTimeout", 408, domain.CodeDeadlineExceeded, http.StatusGatewayTimeout},
		{"409 Conflict", 409, domain.CodeConflict, http.StatusConflict},
		{"422 UnprocessableEntity", 422, domain.CodeInvalidArgument, http.StatusBadRequest},
		{"429 TooManyRequests", 429, domain.CodeResourceExhausted, http.StatusTooManyRequests},
		{"500 InternalServerError", 500, domain.CodeUnavailable, http.StatusServiceUnavailable},
		{"502 BadGateway", 502, domain.CodeUnavailable, http.StatusServiceUnavailable},
		{"503 ServiceUnavailable", 503, domain.CodeUnavailable, http.StatusServiceUnavailable},
		{"504 GatewayTimeout", 504, domain.CodeDeadlineExceeded, http.StatusGatewayTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &httpStatusErr{status: tc.status, inner: fmt.Errorf("openai: upstream call failed")}
			status, code, _ := MapError(err)
			assert.Equal(t, tc.wantStatus, status, "HTTP status mismatch")
			assert.Equal(t, tc.wantCode, code, "business code mismatch")
		})
	}
}

// TestMapError_UpstreamStructuredStatus confirms that an error
// implementing HTTPStatusError is mapped via the structured path.
func TestMapError_UpstreamStructuredStatus(t *testing.T) {
	err := &httpStatusErr{
		status: 429,
		inner:  fmt.Errorf("openai: rate limit hit"),
	}
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, domain.CodeResourceExhausted, code)
	assert.Contains(t, msg, "Upstream model quota or rate limit exceeded")
	assert.Contains(t, msg, "HTTP 429")
}

// TestMapError_UpstreamTextStatus confirms that an error message
// containing an HTTP status pattern is mapped via the text path.
func TestMapError_UpstreamTextStatus(t *testing.T) {
	err := fmt.Errorf("openai: HTTP 429 too many requests")
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, domain.CodeResourceExhausted, code)
	assert.Contains(t, msg, "Upstream model quota or rate limit exceeded")
}

// TestMapError_UpstreamMarkerInvalidAPIKey confirms that an
// "invalid api key" error maps to 401 UNAUTHENTICATED.
func TestMapError_UpstreamMarkerInvalidAPIKey(t *testing.T) {
	err := fmt.Errorf("openai: invalid api key")
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, domain.CodeUnauthenticated, code)
	assert.Contains(t, msg, "Upstream model authentication failed")
}

// TestMapError_UpstreamMarkerForbidden confirms that a "forbidden"
// error from an upstream provider maps to 403 PERMISSION_DENIED.
func TestMapError_UpstreamMarkerForbidden(t *testing.T) {
	err := fmt.Errorf("volcengine: forbidden by policy")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, domain.CodePermissionDenied, code)
}

// TestMapError_UpstreamMarkerRateLimit confirms that a rate-limit
// error from an upstream provider maps to 429 RESOURCE_EXHAUSTED.
func TestMapError_UpstreamMarkerRateLimit(t *testing.T) {
	err := fmt.Errorf("openai: too many requests")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, domain.CodeResourceExhausted, code)
}

// TestMapError_UpstreamAPIKeyConfiguration confirms that an error
// indicating a missing API key configuration maps to 412
// FAILED_PRECONDITION.
func TestMapError_UpstreamAPIKeyConfiguration(t *testing.T) {
	err := fmt.Errorf("vlm configuration: requires 'api_key'")
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusPreconditionFailed, status)
	assert.Equal(t, domain.CodeFailedPrecondition, code)
	assert.Contains(t, msg, "API key is not configured")
}

// TestMapError_UpstreamNotUpstreamError confirms that a generic
// error without upstream markers is NOT claimed by the upstream
// mapper.
func TestMapError_UpstreamNotUpstreamError(t *testing.T) {
	err := errors.New("random error")
	status, code, _ := MapError(err)
	// Should fall through to the generic 500.
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, domain.CodeInternalError, code)
}

// TestMapError_AGFSSentinelNotFound confirms that a domain.ErrNotFound
// sentinel (used by ragfs backends) maps to 404 RESOURCE_NOT_FOUND.
func TestMapError_AGFSSentinelNotFound(t *testing.T) {
	err := domain.Wrap(domain.CodeResourceNotFound, 404,
		errors.New("path /foo/bar not found"))
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusNotFound, status)
	assert.Equal(t, domain.CodeResourceNotFound, code)
}

// TestMapError_AGFSSentinelConflict confirms that a domain.ErrConflict
// sentinel maps to 409 CONFLICT.
func TestMapError_AGFSSentinelConflict(t *testing.T) {
	err := domain.Wrap(domain.CodeConflict, 409,
		errors.New("resource already exists"))
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, domain.CodeConflict, code)
}

// TestMapError_AGFSReadOnly confirms that a read-only error maps to
// 403 FORBIDDEN.
func TestMapError_AGFSReadOnly(t *testing.T) {
	err := errors.New("backend is read-only")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, domain.CodeForbidden, code)
}

// TestMapError_AGFSQuota confirms that a quota / insufficient-storage
// error maps to 507 INSUFFICIENT_STORAGE.
func TestMapError_AGFSQuota(t *testing.T) {
	err := errors.New("ragfs: insufficient storage on volume")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusInsufficientStorage, status)
	assert.Equal(t, domain.CodeInsufficientStorage, code)
}

// TestMapError_AGFSConnectionTimeout confirms that a connection /
// timeout error from a ragfs backend maps to 503 UNAVAILABLE.
func TestMapError_AGFSConnectionTimeout(t *testing.T) {
	err := errors.New("ragfs: connection refused to backend")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusServiceUnavailable, status)
	assert.Equal(t, domain.CodeUnavailable, code)
}

// TestMapError_AGFSIsADirectory confirms that an "is a directory"
// error maps to 400 INVALID_ARGUMENT.
func TestMapError_AGFSIsADirectory(t *testing.T) {
	err := errors.New("ragfs: path is a directory")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, domain.CodeInvalidArgument, code)
}

// TestMapError_AGFSNotADirectory confirms that a "not a directory"
// error maps to 412 FAILED_PRECONDITION.
func TestMapError_AGFSNotADirectory(t *testing.T) {
	err := errors.New("ragfs: path is not a directory")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusPreconditionFailed, status)
	assert.Equal(t, domain.CodeFailedPrecondition, code)
}

// TestMapError_AGFSInvalidURI confirms that an invalid-URI error
// maps to 400 INVALID_URI.
func TestMapError_AGFSInvalidURI(t *testing.T) {
	err := errors.New("invalid viking:// uri")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, domain.CodeInvalidURI, code)
}

// TestMapError_LockAcquire confirms that a lock-acquisition error
// maps to 409 CONFLICT.
func TestMapError_LockAcquire(t *testing.T) {
	err := errors.New("failed to acquire lock on /foo/bar: resource busy")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusConflict, status)
	assert.Equal(t, domain.CodeConflict, code)
}

// TestMapError_LockRefresh confirms that a lock-refresh / lease
// expired error maps to 423 LOCK_CONFLICT.
func TestMapError_LockRefresh(t *testing.T) {
	err := errors.New("lock refresh failed: lease expired")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusLocked, status)
	assert.Equal(t, domain.CodeLockConflict, code)
}

// TestMapError_LockConcurrentCommit confirms that a concurrent
// commit / CAS mismatch error maps to 423 LOCK_CONFLICT.
func TestMapError_LockConcurrentCommit(t *testing.T) {
	err := errors.New("git ref cas mismatch: concurrent commit detected")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusLocked, status)
	assert.Equal(t, domain.CodeLockConflict, code)
}

// TestMapError_SSEConnectionDrop confirms that an SSE connection
// drop maps to 500 SSE_ERROR with a retry-after hint.
func TestMapError_SSEConnectionDrop(t *testing.T) {
	err := errors.New("sse: connection dropped by upstream")
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, domain.CodeSSEError, code)
	assert.Contains(t, msg, "retry-after")
}

// TestMapError_SSEMalformedEvent confirms that a malformed-event
// error maps to 500 SSE_ERROR.
func TestMapError_SSEMalformedEvent(t *testing.T) {
	err := errors.New("sse: malformed event at byte offset 42")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Equal(t, domain.CodeSSEError, code)
}

// TestMapError_OAuthInvalidGrant confirms that an invalid_grant
// error maps to 401 OAUTH_ERROR with a WWW-Authenticate hint.
func TestMapError_OAuthInvalidGrant(t *testing.T) {
	err := errors.New("oauth: invalid_grant - token expired or revoked")
	status, code, msg := MapError(err)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, domain.CodeOAuthError, code)
	assert.Contains(t, msg, "WWW-Authenticate")
	assert.Contains(t, msg, "invalid_token")
}

// TestMapError_OAuthTokenRevoked confirms that a revoked-token
// error maps to 401 OAUTH_ERROR.
func TestMapError_OAuthTokenRevoked(t *testing.T) {
	err := errors.New("oauth: token revoked by resource owner")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, domain.CodeOAuthError, code)
}

// TestMapError_OAuthInvalidClient confirms that an invalid_client
// error maps to 401 UNAUTHENTICATED.
func TestMapError_OAuthInvalidClient(t *testing.T) {
	err := errors.New("oauth: invalid_client - authentication failed")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusUnauthorized, status)
	assert.Equal(t, domain.CodeUnauthenticated, code)
}

// TestMapError_OAuthInvalidRequest confirms that an invalid_request
// error maps to 400 INVALID_ARGUMENT.
func TestMapError_OAuthInvalidRequest(t *testing.T) {
	err := errors.New("oauth: invalid_request - missing grant_type")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, domain.CodeInvalidArgument, code)
}

// TestMapError_OAuthAccessDenied confirms that an access_denied
// error maps to 403 PERMISSION_DENIED.
func TestMapError_OAuthAccessDenied(t *testing.T) {
	err := errors.New("oauth: access_denied - user denied consent")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusForbidden, status)
	assert.Equal(t, domain.CodePermissionDenied, code)
}

// TestMapError_ValidationMissingField confirms that a missing-field
// error maps to 400 INVALID_ARGUMENT.
func TestMapError_ValidationMissingField(t *testing.T) {
	err := errors.New("missing required field: name")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, domain.CodeInvalidArgument, code)
}

// TestMapError_ValidationMustNotBeEmpty confirms that a "must not
// be empty" error maps to 400 INVALID_ARGUMENT.
func TestMapError_ValidationMustNotBeEmpty(t *testing.T) {
	err := errors.New("query must not be empty")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, domain.CodeInvalidArgument, code)
}

// TestMapError_ValidationBadFormat confirms that a bad-format error
// maps to 400 INVALID_ARGUMENT.
func TestMapError_ValidationBadFormat(t *testing.T) {
	err := errors.New("bad format: expected UUID")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, domain.CodeInvalidArgument, code)
}

// TestMapError_ValidationInvalidURI confirms that an invalid-URI
// validation error maps to 400 INVALID_URI.
func TestMapError_ValidationInvalidURI(t *testing.T) {
	err := errors.New("invalid viking uri: missing scheme")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusBadRequest, status)
	assert.Equal(t, domain.CodeInvalidURI, code)
}

// TestMapError_PriorityAppErrorOverUpstream confirms that when an
// error is both an *AppError and looks upstream, the AppError code
// wins. This is the "do not remove existing mappings" guarantee.
func TestMapError_PriorityAppErrorOverUpstream(t *testing.T) {
	inner := fmt.Errorf("openai: HTTP 429 too many requests")
	err := domain.Wrap(domain.CodeVLMFailed, 502, inner)
	status, code, _ := MapError(err)
	// AppError passes through with its explicit code.
	assert.Equal(t, http.StatusBadGateway, status)
	assert.Equal(t, domain.CodeVLMFailed, code)
}

// TestMapError_PriorityUpstreamOverAGFS confirms that the upstream
// mapper is tried before the AGFS mapper when an error looks
// upstream but contains AGFS-flavored text. This matches the Python
// isinstance order.
func TestMapError_PriorityUpstreamOverAGFS(t *testing.T) {
	// "openai" marker + "rate limit" marker -> upstream wins.
	err := fmt.Errorf("openai: rate limit exceeded (ragfs backend timeout)")
	status, code, _ := MapError(err)
	assert.Equal(t, http.StatusTooManyRequests, status)
	assert.Equal(t, domain.CodeResourceExhausted, code)
}

// TestUpstreamCodeForStatus confirms the helper returns the right
// code for known and unknown statuses.
func TestUpstreamCodeForStatus(t *testing.T) {
	assert.Equal(t, domain.CodeInvalidArgument, upstreamCodeForStatus(400))
	assert.Equal(t, domain.CodeResourceExhausted, upstreamCodeForStatus(429))
	assert.Equal(t, domain.CodeUnavailable, upstreamCodeForStatus(500))
	assert.Equal(t, domain.CodeUnavailable, upstreamCodeForStatus(503))
	assert.Equal(t, domain.CodeDeadlineExceeded, upstreamCodeForStatus(504))
	// Unknown 4xx -> INVALID_ARGUMENT; unknown 5xx -> UNAVAILABLE.
	assert.Equal(t, domain.CodeInvalidArgument, upstreamCodeForStatus(418))
	assert.Equal(t, domain.CodeUnavailable, upstreamCodeForStatus(599))
	// Out of range -> empty string.
	assert.Equal(t, "", upstreamCodeForStatus(200))
	assert.Equal(t, "", upstreamCodeForStatus(600))
}

// TestCoerceHTTPStatus confirms the helper validates the HTTP
// status range.
func TestCoerceHTTPStatus(t *testing.T) {
	assert.Equal(t, 200, coerceHTTPStatus(200))
	assert.Equal(t, 429, coerceHTTPStatus(429))
	assert.Equal(t, 599, coerceHTTPStatus(599))
	assert.Equal(t, 0, coerceHTTPStatus(99))
	assert.Equal(t, 0, coerceHTTPStatus(600))
	assert.Equal(t, 0, coerceHTTPStatus(0))
}

// TestTrimMessage confirms the helper truncates long messages with
// an ellipsis and passes short messages through.
func TestTrimMessage(t *testing.T) {
	short := "hello world"
	assert.Equal(t, short, trimMessage(short))

	long := strings.Repeat("a", 600)
	got := trimMessage(long)
	require.Len(t, got, 500)
	assert.True(t, strings.HasSuffix(got, "..."))

	// Custom limit.
	got = trimMessage(long, 100)
	require.Len(t, got, 100)
	assert.True(t, strings.HasSuffix(got, "..."))
}

// TestNormalizeMessage confirms the helper collapses whitespace.
func TestNormalizeMessage(t *testing.T) {
	assert.Equal(t, "hello world", normalizeMessage("  hello   world  "))
	assert.Equal(t, "a b c", normalizeMessage("a\n\tb\n\tc"))
	assert.Equal(t, "", normalizeMessage("   "))
}

// TestContainsAny confirms the helper.
func TestContainsAny(t *testing.T) {
	assert.True(t, containsAny("hello world", []string{"world"}))
	assert.True(t, containsAny("hello world", []string{"xyz", "world"}))
	assert.False(t, containsAny("hello world", []string{"xyz"}))
	assert.False(t, containsAny("hello", []string{""})) // empty substr skipped
}

// TestWordBoundaryContains confirms the helper.
func TestWordBoundaryContains(t *testing.T) {
	assert.True(t, wordBoundaryContains("error 429 too many", "429"))
	assert.False(t, wordBoundaryContains("error 4290 too many", "429"))
	assert.True(t, wordBoundaryContains("429 too many", "429"))
	assert.True(t, wordBoundaryContains("error 429", "429"))
	assert.False(t, wordBoundaryContains("error 42", "429"))
}

// TestLooksLikeUpstreamModelError confirms the marker check.
func TestLooksLikeUpstreamModelError(t *testing.T) {
	assert.True(t, looksLikeUpstreamModelError(fmt.Errorf("openai: rate limit")))
	assert.True(t, looksLikeUpstreamModelError(fmt.Errorf("volcengine: forbidden")))
	assert.True(t, looksLikeUpstreamModelError(fmt.Errorf("upstream service unavailable")))
	assert.False(t, looksLikeUpstreamModelError(fmt.Errorf("random error")))
	assert.False(t, looksLikeUpstreamModelError(nil))
}

// TestIsModelAPIKeyConfigurationError confirms the config-error
// detector.
func TestIsModelAPIKeyConfigurationError(t *testing.T) {
	assert.True(t, isModelAPIKeyConfigurationError(fmt.Errorf("vlm configuration: requires 'api_key'")))
	assert.True(t, isModelAPIKeyConfigurationError(fmt.Errorf("openai provider: api_key is required")))
	assert.False(t, isModelAPIKeyConfigurationError(fmt.Errorf("invalid api key")))
	assert.False(t, isModelAPIKeyConfigurationError(fmt.Errorf("unauthorized: invalid api key")))
	assert.False(t, isModelAPIKeyConfigurationError(fmt.Errorf("random error")))
}

// TestExceptionChainText confirms the chain walker joins messages
// and de-duplicates.
func TestExceptionChainText(t *testing.T) {
	inner := errors.New("inner")
	mid := fmt.Errorf("mid: %w", inner)
	outer := fmt.Errorf("outer: %w", mid)
	text := exceptionChainText(outer)
	assert.Contains(t, text, "outer")
	assert.Contains(t, text, "mid")
	assert.Contains(t, text, "inner")
}

// TestBuildUpstreamMessage confirms the message builder renders the
// label + status + detail.
func TestBuildUpstreamMessage(t *testing.T) {
	msg := buildUpstreamMessage(domain.CodeResourceExhausted, 429, "rate limit hit")
	assert.Contains(t, msg, "Upstream model quota or rate limit exceeded")
	assert.Contains(t, msg, "HTTP 429")
	assert.Contains(t, msg, "rate limit hit")

	msg = buildUpstreamMessage(domain.CodeUnavailable, 0, "")
	assert.Contains(t, msg, "Upstream model service unavailable")
	assert.NotContains(t, msg, "HTTP")
}

// TestHTTPStatusErrInterface confirms the test double implements
// HTTPStatusError.
func TestHTTPStatusErrInterface(t *testing.T) {
	var _ HTTPStatusError = (*httpStatusErr)(nil)
}
