// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package server

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// error_mapping.go is the Go port of openviking/server/error_mapping.py.
// It translates errors raised by upstream providers (LLM/VL embedder,
// vikingdb, ragfs, lock manager, SSE stream, OAuth provider) into the
// (HTTP status, business code, message) triple the API returns to
// clients. The top-level entry point is MapError; errorMiddleware calls
// it for any non-AppError it sees in c.Errors so upstream failures are
// rendered with the correct 4xx/5xx code instead of a generic 500.
//
// The Python file is 592 lines and covers six categories:
//
//  1. Upstream HTTP errors (429/5xx from LLM/VL/embedder/vikingdb)
//  2. AGFS errors (not found, conflict, read-only, quota)
//  3. Lock errors (acquire, refresh, lease)
//  4. SSE errors (connection drop, malformed event)
//  5. OAuth errors (invalid_grant, expired token, revoked)
//  6. Validation errors (missing field, bad format)
//
// Each category has its own mapper (mapUpstreamAPIError, mapAGFSError,
// mapLockError, mapSSEError, mapOAuthError, mapValidationError). The
// top-level MapError tries them in priority order and falls back to
// 500 INTERNAL_ERROR when no mapper claims the error.

// HTTPStatusError is the interface an error can implement to expose an
// upstream HTTP status code. The mapUpstreamAPIError mapper checks for
// it via errors.As so provider clients (vlm, vectordb, etc.) can attach
// a status without depending on this package.
type HTTPStatusError interface {
	HTTPStatus() int
}

// upstreamHTTPStatusToCode maps upstream HTTP status codes to API error
// codes. Mirrors Python _UPSTREAM_HTTP_STATUS_TO_ERROR_CODE. Statuses
// not in this map fall through to INVALID_ARGUMENT (4xx) or UNAVAILABLE
// (5xx) in upstreamCodeForStatus.
var upstreamHTTPStatusToCode = map[int]string{
	400: domain.CodeInvalidArgument,
	401: domain.CodeUnauthenticated,
	402: domain.CodeResourceExhausted,
	403: domain.CodePermissionDenied,
	404: domain.CodeNotFound,
	408: domain.CodeDeadlineExceeded,
	409: domain.CodeConflict,
	422: domain.CodeInvalidArgument,
	429: domain.CodeResourceExhausted,
	500: domain.CodeUnavailable,
	502: domain.CodeUnavailable,
	503: domain.CodeUnavailable,
	504: domain.CodeDeadlineExceeded,
}

// upstreamErrorMarkers lists substrings that mark an error as
// originating from an upstream model provider (LLM, VL embedder,
// vikingdb, ragfs backend). Mirrors Python _UPSTREAM_ERROR_MARKERS.
// Matching is case-insensitive against the joined error chain text.
var upstreamErrorMarkers = []string{
	"api error",
	"apierror",
	"badrequesterror",
	"authenticationerror",
	"permissiondeniederror",
	"ratelimiterror",
	"httpstatuserror",
	"openai",
	"litellm",
	"volcengine",
	"ark",
	"gemini",
	"jina",
	"voyage",
	"cohere",
	"dashscope",
	"minimax",
	"embedding",
	"embedder",
	"vlm",
	"model",
	"upstream",
	"invalid api key",
	"unauthorized",
	"forbidden",
	"too many requests",
	"rate limit",
	"quota",
	"accountoverdue",
}

// apiKeyConfigMarkers lists substrings that mark an error as a
// model-provider API-key configuration error (key missing, key empty,
// key not set). Mirrors Python _API_KEY_CONFIG_MARKERS.
var apiKeyConfigMarkers = []string{
	"vlm configuration",
	"embedding",
	"embedder",
	"provider",
	"openai",
	"azure",
	"volcengine",
	"jina",
	"gemini",
	"voyage",
	"dashscope",
	"minimax",
	"cohere",
}

// apiKeyMissingPhrases are the exact phrases that, when present in an
// upstream-looking error, identify it as a configuration error rather
// than a runtime auth failure.
var apiKeyMissingPhrases = []string{
	"requires 'api_key'",
	"requires api_key",
	"api_key is required",
	"api key is required",
	"requires 'api key'",
}

// httpStatusPatterns are the regexes used to extract an HTTP status
// code from the text of an upstream error message. Mirrors Python
// _HTTP_STATUS_PATTERNS. They run against the joined chain text; the
// first match wins.
var httpStatusPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bHTTP\s*(\d{3})\b`),
	regexp.MustCompile(`(?i)\bstatus(?:\s+code)?\s*[:=]?\s*(\d{3})\b`),
	regexp.MustCompile(`(?i)\berror\s+code\s*[:=]?\s*(\d{3})\b`),
}

// knownHTTPStatuses lists every status that appears in
// upstreamHTTPStatusToCode. Used by extractTextHTTPStatus as a last
// resort when the structured status is unavailable.
var knownHTTPStatuses = func() []int {
	out := make([]int, 0, len(upstreamHTTPStatusToCode))
	for s := range upstreamHTTPStatusToCode {
		out = append(out, s)
	}
	return out
}()

// MapError is the top-level error mapper. It inspects err (and its
// unwrap chain) and returns the (HTTP status, business code, message)
// triple that should be rendered to the client.
//
// Returns (500, INTERNAL_ERROR, err.Error()) when no mapper claims the
// error, so callers can render a generic 500 without an extra branch.
// Passes *domain.AppError through unchanged so handlers that already
// built a structured error keep their explicit code/status.
//
// Mirrors Python map_exception; the per-category mappers below mirror
// the isinstance branches.
func MapError(err error) (status int, code string, message string) {
	if err == nil {
		return http.StatusInternalServerError, domain.CodeInternalError, "internal error"
	}

	// AppError passes through with its own code/status/details. This
	// preserves the explicit choices handlers made via domain.Wrap.
	var appErr *domain.AppError
	if errors.As(err, &appErr) {
		return appErrStatus(appErr), appErr.Code, appErrMsg(appErr)
	}

	// Try each category mapper in priority order. The order mirrors the
	// Python isinstance chain: structured AppError first, then upstream
	// (most specific, since it has its own status extraction), then
	// AGFS, lock, SSE, OAuth, validation. The first mapper to claim the
	// error wins.
	if s, c, m, ok := mapUpstreamAPIError(err); ok {
		return s, c, m
	}
	if s, c, m, ok := mapAGFSError(err); ok {
		return s, c, m
	}
	if s, c, m, ok := mapLockError(err); ok {
		return s, c, m
	}
	if s, c, m, ok := mapSSEError(err); ok {
		return s, c, m
	}
	if s, c, m, ok := mapOAuthError(err); ok {
		return s, c, m
	}
	if s, c, m, ok := mapValidationError(err); ok {
		return s, c, m
	}

	// Generic fallback. Matches Python's "return None" path where the
	// caller (here, the middleware) renders a 500 INTERNAL_ERROR.
	return http.StatusInternalServerError, domain.CodeInternalError, err.Error()
}

// appErrStatus returns the HTTP status for an AppError, defaulting to
// 500 when unset. Mirrors the Python OpenVikingError.status field.
func appErrStatus(e *domain.AppError) int {
	if e.Status == 0 {
		return http.StatusInternalServerError
	}
	return e.Status
}

// appErrMsg returns the human-readable message for an AppError. Falls
// back to the code itself when no underlying error is attached.
func appErrMsg(e *domain.AppError) string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

// ---------------------------------------------------------------------------
// Upstream HTTP / model provider errors. Mirrors Python
// _map_upstream_api_error + helpers.
// ---------------------------------------------------------------------------

// mapUpstreamAPIError translates an error from an LLM/VL/embedder/
// vikingdb provider into an API error triple. Returns ok=false when
// the error does not look upstream.
func mapUpstreamAPIError(err error) (status int, code string, message string, ok bool) {
	if err == nil {
		return 0, "", "", false
	}

	// 1. API-key configuration error -> 412 FAILED_PRECONDITION. This
	//    is distinct from "invalid api key" (which is a 401 auth
	//    failure) and lets the operator distinguish "configure the
	//    provider" from "rotate the key".
	if isModelAPIKeyConfigurationError(err) {
		msg := trimMessage(exceptionChainText(err))
		return http.StatusPreconditionFailed, domain.CodeFailedPrecondition,
			"Model provider API key is not configured: " + msg, true
	}

	// 2. Structured HTTP status via HTTPStatusError interface.
	if status, source, found := extractStructuredHTTPStatus(err); found {
		code := upstreamCodeForStatus(status)
		if code == "" {
			// Unknown status code; let the next mapper try.
			_ = source
		} else {
			msg := trimMessage(upstreamDetailMessage(err))
			return httpStatusForUpstreamCode(code), code,
				buildUpstreamMessage(code, status, msg), true
		}
	}

	// 3. Text-extracted HTTP status from the error message.
	if status := extractTextHTTPStatus(err); status != 0 {
		code := upstreamCodeForStatus(status)
		if code != "" {
			msg := trimMessage(upstreamDetailMessage(err))
			return httpStatusForUpstreamCode(code), code,
				buildUpstreamMessage(code, status, msg), true
		}
	}

	// 4. Marker-based classification when no status is available.
	if !looksLikeUpstreamModelError(err) {
		return 0, "", "", false
	}
	lowered := strings.ToLower(exceptionChainText(err))
	switch {
	case strings.Contains(lowered, "invalid api key") || strings.Contains(lowered, "unauthorized"):
		msg := trimMessage(upstreamDetailMessage(err))
		return http.StatusUnauthorized, domain.CodeUnauthenticated,
			buildUpstreamMessage(domain.CodeUnauthenticated, 0, msg), true
	case strings.Contains(lowered, "forbidden"):
		msg := trimMessage(upstreamDetailMessage(err))
		return http.StatusForbidden, domain.CodePermissionDenied,
			buildUpstreamMessage(domain.CodePermissionDenied, 0, msg), true
	case strings.Contains(lowered, "too many requests") ||
		strings.Contains(lowered, "rate limit") ||
		strings.Contains(lowered, "ratelimit") ||
		strings.Contains(lowered, "quota") ||
		strings.Contains(lowered, "accountoverdue") ||
		strings.Contains(lowered, "resource exhausted"):
		msg := trimMessage(upstreamDetailMessage(err))
		return http.StatusTooManyRequests, domain.CodeResourceExhausted,
			buildUpstreamMessage(domain.CodeResourceExhausted, 0, msg), true
	}
	return 0, "", "", false
}

// upstreamCodeForStatus maps an HTTP status to an upstream error code.
// Returns "" for statuses outside the 4xx/5xx range. Mirrors Python
// _upstream_code_for_status.
func upstreamCodeForStatus(status int) string {
	if c, ok := upstreamHTTPStatusToCode[status]; ok {
		return c
	}
	switch {
	case 400 <= status && status < 500:
		return domain.CodeInvalidArgument
	case 500 <= status && status < 600:
		return domain.CodeUnavailable
	}
	return ""
}

// httpStatusForUpstreamCode returns the HTTP status the API should
// render for the given upstream code. It is the inverse of
// upstreamHTTPStatusToCode for the common codes; for codes that have
// no upstream status attached (e.g. marker-classified) it returns the
// canonical status from the domain sentinel.
func httpStatusForUpstreamCode(code string) int {
	switch code {
	case domain.CodeInvalidArgument:
		return http.StatusBadRequest
	case domain.CodeUnauthenticated:
		return http.StatusUnauthorized
	case domain.CodePermissionDenied:
		return http.StatusForbidden
	case domain.CodeNotFound:
		return http.StatusNotFound
	case domain.CodeDeadlineExceeded:
		return http.StatusGatewayTimeout
	case domain.CodeConflict:
		return http.StatusConflict
	case domain.CodeResourceExhausted:
		return http.StatusTooManyRequests
	case domain.CodeUnavailable:
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// buildUpstreamMessage renders the human-readable message for an
// upstream-mapped error. Mirrors Python _build_upstream_error's
// `display` field, with the same labels.
func buildUpstreamMessage(code string, status int, detail string) string {
	labels := map[string]string{
		domain.CodeInvalidArgument:   "Upstream model request was rejected",
		domain.CodeUnauthenticated:   "Upstream model authentication failed",
		domain.CodePermissionDenied:  "Upstream model permission denied",
		domain.CodeNotFound:          "Upstream model resource not found",
		domain.CodeConflict:          "Upstream model request conflicted",
		domain.CodeResourceExhausted: "Upstream model quota or rate limit exceeded",
		domain.CodeDeadlineExceeded:  "Upstream model request timed out",
		domain.CodeUnavailable:       "Upstream model service unavailable",
	}
	display, ok := labels[code]
	if !ok {
		display = "Upstream model error"
	}
	if status != 0 {
		display = fmt.Sprintf("%s (HTTP %d)", display, status)
	}
	if detail != "" {
		display = display + ": " + detail
	}
	return display
}

// extractStructuredHTTPStatus walks the error chain and returns the
// first HTTP status it finds via the HTTPStatusError interface.
// Mirrors Python _extract_structured_http_status.
func extractStructuredHTTPStatus(err error) (int, error, bool) {
	current := err
	for current != nil {
		var hse HTTPStatusError
		if errors.As(current, &hse) {
			if s := coerceHTTPStatus(hse.HTTPStatus()); s != 0 {
				return s, current, true
			}
		}
		// Also unwrap plain errors so wrapped sentinels are visible.
		current = errors.Unwrap(current)
	}
	return 0, nil, false
}

// extractTextHTTPStatus scans the error message for an HTTP status
// pattern. Mirrors Python _extract_text_http_status.
func extractTextHTTPStatus(err error) int {
	if !looksLikeUpstreamModelError(err) {
		return 0
	}
	text := exceptionChainText(err)
	for _, p := range httpStatusPatterns {
		if m := p.FindStringSubmatch(text); m != nil {
			if s := coerceHTTPStatus(atoi(m[1])); s != 0 {
				return s
			}
		}
	}
	lowered := strings.ToLower(text)
	for _, s := range knownHTTPStatuses {
		if wordBoundaryContains(lowered, strconv.Itoa(s)) {
			return s
		}
	}
	return 0
}

// looksLikeUpstreamModelError reports whether the error chain text
// contains any of the upstreamErrorMarkers. Mirrors Python
// _looks_like_upstream_model_error.
func looksLikeUpstreamModelError(err error) bool {
	if err == nil {
		return false
	}
	lowered := strings.ToLower(exceptionChainText(err))
	for _, marker := range upstreamErrorMarkers {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// isModelAPIKeyConfigurationError reports whether the error indicates
// the model provider API key was not configured. Mirrors Python
// _is_model_api_key_configuration_error.
func isModelAPIKeyConfigurationError(err error) bool {
	if err == nil {
		return false
	}
	lowered := strings.ToLower(exceptionChainText(err))
	if !strings.Contains(lowered, "api_key") && !strings.Contains(lowered, "api key") {
		return false
	}
	if strings.Contains(lowered, "invalid api key") || strings.Contains(lowered, "unauthorized") {
		return false
	}
	if !containsAny(lowered, apiKeyMissingPhrases) {
		return false
	}
	if containsAny(lowered, apiKeyConfigMarkers) {
		return true
	}
	return strings.TrimSpace(lowered) == "api_key is required" ||
		strings.TrimSpace(lowered) == "api key is required"
}

// upstreamDetailMessage returns the message text used to populate the
// detail field of an upstream-mapped error. Mirrors Python
// _upstream_detail_message.
func upstreamDetailMessage(err error) string {
	return exceptionChainText(err)
}

// ---------------------------------------------------------------------------
// AGFS / ragfs errors. Mirrors the AGFS isinstance branches of
// Python map_exception.
// ---------------------------------------------------------------------------

// mapAGFSError translates a ragfs backend error into an API error
// triple. Returns ok=false when the error is not ragfs-flavored.
//
// The Go ragfs package uses domain.ErrNotFound / domain.ErrConflict /
// ragfs.ErrUnsupported / ragfs.ErrReadOnly / ragfs.ErrAlreadyExists
// sentinels (see internal/ragfs/filesystem.go). Backend plugins
// (memfs, localfs, s3fs) wrap OS errors into these sentinels so
// errors.Is works uniformly. This mapper recognizes those sentinels
// and the AGFS-flavored substrings ("not a directory", "directory not
// empty", "permission denied", etc.) from the Python reference.
func mapAGFSError(err error) (status int, code string, message string, ok bool) {
	if err == nil {
		return 0, "", "", false
	}

	// ragfs sentinel errors. These are *domain.AppError values wrapped
	// via domain.Wrap; the per-code shape is checked before the message
	// scan so explicit sentinels win over heuristic matching.
	//
	// ragfs.ErrAlreadyExists is a *domain.AppError with CodeConflict,
	// so errors.Is(err, domain.ErrConflict) matches both the explicit
	// domain.ErrConflict sentinel and any ragfs-flavored conflict.
	// AppError.Is compares by code, not by pointer identity.
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return http.StatusNotFound, domain.CodeResourceNotFound,
			err.Error(), true
	case errors.Is(err, domain.ErrConflict):
		return http.StatusConflict, domain.CodeConflict,
			err.Error(), true
	}

	// ragfs.ErrReadOnly is a domain.AppError with CodeForbidden; check
	// it before the generic forbidden branch so the message survives.
	if errors.Is(err, ragfsErrReadOnly()) {
		return http.StatusForbidden, domain.CodeForbidden,
			err.Error(), true
	}

	// Substring heuristics for backend errors that surface as plain
	// errors (e.g. "vectordb: collection not found"). These mirror the
	// AGFSClientError branch in Python map_exception.
	msg := err.Error()
	lowered := strings.ToLower(msg)
	switch {
	case containsAny(lowered, []string{"not found", "no such file", "does not exist", "collection not found", "row not found"}):
		return http.StatusNotFound, domain.CodeResourceNotFound, msg, true
	case containsAny(lowered, []string{"already exists", "already exist"}):
		return http.StatusConflict, domain.CodeConflict, msg, true
	case strings.Contains(lowered, "not a directory"):
		return http.StatusPreconditionFailed, domain.CodeFailedPrecondition, msg, true
	case strings.Contains(lowered, "is a directory"):
		return http.StatusBadRequest, domain.CodeInvalidArgument, msg, true
	case strings.Contains(lowered, "directory not empty"):
		return http.StatusPreconditionFailed, domain.CodeFailedPrecondition, msg, true
	case strings.Contains(lowered, "permission denied") || strings.Contains(lowered, "access denied"):
		return http.StatusForbidden, domain.CodeForbidden, msg, true
	case strings.Contains(lowered, "read-only") || strings.Contains(lowered, "readonly"):
		return http.StatusForbidden, domain.CodeForbidden, msg, true
	case strings.Contains(lowered, "invalid uri") || strings.Contains(lowered, "invalid viking uri") || strings.Contains(lowered, "invalid viking://"):
		return http.StatusBadRequest, domain.CodeInvalidURI, msg, true
	case strings.Contains(lowered, "invalid operation") ||
		strings.Contains(lowered, "regex parse error") ||
		strings.Contains(lowered, "invalid regular expression"):
		return http.StatusBadRequest, domain.CodeInvalidArgument, msg, true
	case strings.Contains(lowered, "quota") || strings.Contains(lowered, "insufficient storage") || strings.Contains(lowered, "no space left"):
		return http.StatusInsufficientStorage, domain.CodeInsufficientStorage, msg, true
	case strings.Contains(lowered, "timeout") || strings.Contains(lowered, "connection refused") || strings.Contains(lowered, "connection reset"):
		return http.StatusServiceUnavailable, domain.CodeUnavailable, msg, true
	}
	return 0, "", "", false
}

// ragfsErrReadOnly returns the ragfs.ErrReadOnly sentinel. It is
// accessed via a helper so this file does not import the ragfs package
// (which would create an import cycle: ragfs imports domain, and the
// server package is imported by ragfs plugins). The sentinel is a
// *domain.AppError with CodeForbidden, so errors.Is against it is the
// same as errors.Is against domain.ErrForbidden; the helper exists
// purely for documentation.
func ragfsErrReadOnly() error {
	// We intentionally match by code, not by pointer identity, because
	// ragfs.ErrReadOnly is a fresh *domain.AppError each time it is
	// returned by domain.Wrap. The Is() implementation on AppError
	// compares by Code, so errors.Is(err, domain.ErrForbidden) covers
	// both the read-only case and the explicit forbidden case.
	return domain.ErrForbidden
}

// ---------------------------------------------------------------------------
// Lock errors. Mirrors Python LockAcquisitionError + ResourceBusyError
// + GitConcurrentCommitError branches.
// ---------------------------------------------------------------------------

// mapLockError translates a lock-contention error into an API error
// triple. Lock acquisition / refresh / lease failures map to 409
// CONFLICT or 423 LOCK_CONFLICT depending on whether the resource is
// just busy (retryable) or held by another writer (locked).
func mapLockError(err error) (status int, code string, message string, ok bool) {
	if err == nil {
		return 0, "", "", false
	}
	lowered := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lowered, "lock acquire") ||
		strings.Contains(lowered, "lock acquisition") ||
		strings.Contains(lowered, "acquire lock") ||
		strings.Contains(lowered, "failed to acquire lock") ||
		strings.Contains(lowered, "resource busy") ||
		strings.Contains(lowered, "path busy") ||
		strings.Contains(lowered, "resourcebusy"):
		return http.StatusConflict, domain.CodeConflict, err.Error(), true
	case strings.Contains(lowered, "lock refresh") ||
		strings.Contains(lowered, "refresh lock") ||
		strings.Contains(lowered, "failed to refresh lock") ||
		strings.Contains(lowered, "lease expired") ||
		strings.Contains(lowered, "lock lease") ||
		strings.Contains(lowered, "held by another") ||
		strings.Contains(lowered, "concurrent commit") ||
		strings.Contains(lowered, "git ref cas") ||
		strings.Contains(lowered, "cas mismatch"):
		return http.StatusLocked, domain.CodeLockConflict, err.Error(), true
	}
	return 0, "", "", false
}

// ---------------------------------------------------------------------------
// SSE errors. Mirrors the SSE-flavored branches of the Python
// error_mapping surface.
// ---------------------------------------------------------------------------

// mapSSEError translates an SSE-stream error into an API error triple.
// Connection drops and malformed events surface as 500 SSE_ERROR with
// a Retry-After hint in the message. The Go SSE stack (net/http +
// gin) does not expose a specific SSE-parser exception, so this mapper
// relies on substring heuristics; the Python reference's
// eventsource-parser exception is noted as a gap and intentionally
// not stubbed.
func mapSSEError(err error) (status int, code string, message string, ok bool) {
	if err == nil {
		return 0, "", "", false
	}
	lowered := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lowered, "sse:") ||
		strings.Contains(lowered, "eventsource") ||
		strings.Contains(lowered, "event source") ||
		strings.Contains(lowered, "connection drop") ||
		strings.Contains(lowered, "connection closed") ||
		strings.Contains(lowered, "stream closed") ||
		strings.Contains(lowered, "malformed event") ||
		strings.Contains(lowered, "malformed sse") ||
		strings.Contains(lowered, "bad event") ||
		strings.Contains(lowered, "event parse"):
		return http.StatusInternalServerError, domain.CodeSSEError,
			err.Error() + " (retry-after: 5s)", true
	}
	return 0, "", "", false
}

// ---------------------------------------------------------------------------
// OAuth errors. Mirrors the OAuth branches of the Python surface.
// ---------------------------------------------------------------------------

// mapOAuthError translates an OAuth provider error into an API error
// triple. invalid_grant / expired_token / revoked_token map to 401
// OAUTH_ERROR with a WWW-Authenticate hint; invalid_client maps to
// 401 UNAUTHENTICATED; invalid_request maps to 400 INVALID_ARGUMENT.
//
// The Go OAuth stack uses fosite, which returns its own typed errors
// (ErrInvalidRequest, ErrInvalidClient, ErrInvalidGrant, ...). The
// fosite errors implement the RFC 6749 error-code interface; we match
// on the error string because importing fosite here would pull a
// heavy dependency into the server package. The middleware that
// renders OAuth responses (oauth/handlers.go) calls
// fosite.WriteAccessError directly, so this mapper is the fallback
// for any OAuth error that escapes that path.
func mapOAuthError(err error) (status int, code string, message string, ok bool) {
	if err == nil {
		return 0, "", "", false
	}
	lowered := strings.ToLower(err.Error())
	switch {
	case strings.Contains(lowered, "invalid_grant") ||
		strings.Contains(lowered, "invalid grant") ||
		strings.Contains(lowered, "expired token") ||
		strings.Contains(lowered, "token expired") ||
		strings.Contains(lowered, "token revoked") ||
		strings.Contains(lowered, "revoked"):
		return http.StatusUnauthorized, domain.CodeOAuthError,
			err.Error() + " (WWW-Authenticate: Bearer realm=\"openviking\", error=\"invalid_token\")", true
	case strings.Contains(lowered, "invalid_client") ||
		strings.Contains(lowered, "invalid client") ||
		strings.Contains(lowered, "client authentication failed"):
		return http.StatusUnauthorized, domain.CodeUnauthenticated, err.Error(), true
	case strings.Contains(lowered, "invalid_request") ||
		strings.Contains(lowered, "invalid request"):
		return http.StatusBadRequest, domain.CodeInvalidArgument, err.Error(), true
	case strings.Contains(lowered, "unauthorized_client") ||
		strings.Contains(lowered, "unsupported_grant_type") ||
		strings.Contains(lowered, "unsupported grant type"):
		return http.StatusBadRequest, domain.CodeInvalidArgument, err.Error(), true
	case strings.Contains(lowered, "access_denied") ||
		strings.Contains(lowered, "access denied"):
		return http.StatusForbidden, domain.CodePermissionDenied, err.Error(), true
	case strings.Contains(lowered, "insufficient_scope") ||
		strings.Contains(lowered, "insufficient scope"):
		return http.StatusForbidden, domain.CodePermissionDenied, err.Error(), true
	}
	return 0, "", "", false
}

// ---------------------------------------------------------------------------
// Validation errors. Mirrors the ValueError + InvalidURIError +
// FailedPreconditionError branches of the Python surface.
// ---------------------------------------------------------------------------

// mapValidationError translates a validation error into an API error
// triple. Missing-field and bad-format errors map to 400
// INVALID_ARGUMENT; invalid-URI errors map to 400 INVALID_URI. This
// mapper is the last resort before the generic 500 fallback so that
// obvious validation failures surface as 4xx.
func mapValidationError(err error) (status int, code string, message string, ok bool) {
	if err == nil {
		return 0, "", "", false
	}
	msg := err.Error()
	lowered := strings.ToLower(msg)
	switch {
	case strings.Contains(lowered, "missing field") ||
		strings.Contains(lowered, "missing required") ||
		strings.Contains(lowered, "is required") ||
		strings.Contains(lowered, "must not be empty") ||
		strings.Contains(lowered, "must not be null") ||
		strings.Contains(lowered, "bad format") ||
		strings.Contains(lowered, "invalid format") ||
		strings.Contains(lowered, "invalid value"):
		return http.StatusBadRequest, domain.CodeInvalidArgument, msg, true
	case strings.Contains(lowered, "invalid uri") ||
		strings.Contains(lowered, "invalid viking uri") ||
		strings.Contains(lowered, "invalid viking://"):
		return http.StatusBadRequest, domain.CodeInvalidURI, msg, true
	case strings.Contains(lowered, "regex parse error") ||
		strings.Contains(lowered, "invalid regular expression"):
		return http.StatusBadRequest, domain.CodeInvalidArgument, msg, true
	}
	return 0, "", "", false
}

// ---------------------------------------------------------------------------
// Shared helpers. These are the Go equivalents of the Python helper
// functions in error_mapping.py.
// ---------------------------------------------------------------------------

// exceptionChainText walks the error chain via errors.Unwrap and
// returns the joined message text. De-duplicated and whitespace-
// normalized so the upstream markers and HTTP-status regexes match
// cleanly across wrapped errors. Mirrors Python _exception_chain_text.
func exceptionChainText(err error) string {
	if err == nil {
		return ""
	}
	var messages []string
	seen := map[string]bool{}
	current := err
	for current != nil {
		msg := normalizeMessage(current.Error())
		if msg != "" && !seen[msg] {
			seen[msg] = true
			messages = append(messages, msg)
		}
		current = errors.Unwrap(current)
	}
	return strings.Join(dedupeMessages(messages), "\n")
}

// normalizeMessage collapses internal whitespace to single spaces and
// trims leading/trailing space. Mirrors Python _normalize_message.
func normalizeMessage(msg string) string {
	return strings.Join(strings.Fields(msg), " ")
}

// dedupeMessages drops messages that are substrings of other messages
// in the slice. Mirrors Python _dedupe_messages.
func dedupeMessages(messages []string) []string {
	var result []string
	for _, msg := range messages {
		normalized := normalizeMessage(msg)
		if normalized == "" {
			continue
		}
		duplicate := false
		for _, existing := range result {
			if normalized == existing || strings.Contains(existing, normalized) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		// Drop any result entries that are substrings of the new message.
		var kept []string
		for _, existing := range result {
			if !strings.Contains(normalized, existing) {
				kept = append(kept, existing)
			}
		}
		result = append(kept, normalized)
	}
	return result
}

// trimMessage truncates a message to limit characters and appends
// "..." when truncated. Mirrors Python _trim_message (default 500).
func trimMessage(msg string, limit ...int) string {
	maxLen := 500
	if len(limit) > 0 && limit[0] > 0 {
		maxLen = limit[0]
	}
	normalized := normalizeMessage(msg)
	if len(normalized) <= maxLen {
		return normalized
	}
	return normalized[:maxLen-3] + "..."
}

// coerceHTTPStatus returns the status code if it is in the valid HTTP
// range (100-599), 0 otherwise. Mirrors Python _coerce_http_status.
// Booleans are rejected explicitly because Go's int(bool) is well-
// defined but the Python helper rejects them.
func coerceHTTPStatus(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}

// atoi parses a string to an int, returning 0 on error. Used by the
// HTTP-status regex extractor.
func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// containsAny reports whether s contains any of the substrings. Case
// sensitivity is the caller's responsibility (most callers lower s
// first).
func containsAny(s string, substrs []string) bool {
	for _, sub := range substrs {
		if sub == "" {
			continue
		}
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// wordBoundaryContains reports whether s contains the given word with
// word boundaries on both sides. Used to avoid matching "4290" when
// scanning for "429".
func wordBoundaryContains(s, word string) bool {
	if s == "" || word == "" {
		return false
	}
	idx := strings.Index(s, word)
	for idx >= 0 {
		left := idx == 0 || !isWordChar(s[idx-1])
		rightEnd := idx + len(word)
		right := rightEnd == len(s) || !isWordChar(s[rightEnd])
		if left && right {
			return true
		}
		next := idx + 1
		if next >= len(s) {
			return false
		}
		idx = strings.Index(s[next:], word)
		if idx < 0 {
			return false
		}
		idx = next + idx
	}
	return false
}

// isWordChar reports whether b is a word character (letter, digit, or
// underscore). Used by wordBoundaryContains.
func isWordChar(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_'
}
