package domain

import (
	"errors"
	"fmt"
)

// AppError is the canonical error type returned by every internal function.
// It is mapped to an HTTP response by internal/server/middleware/error.go.
// Code must be one of the *_CODE constants below to stay aligned with the
// Python error_mapping.py.
type AppError struct {
	Code    string         // business error code, e.g. "RESOURCE_NOT_FOUND"
	Status  int            // HTTP status code
	Err     error          // wrapped underlying error
	Details map[string]any // optional structured details for the response body
}

// Error implements the error interface.
func (e *AppError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Code, e.Err)
	}
	return e.Code
}

// Unwrap exposes the underlying error for errors.Is / errors.As.
func (e *AppError) Unwrap() error { return e.Err }

// Is reports whether target is an AppError with the same business code as
// e. This lets callers use errors.Is(err, domain.ErrNotFound) to match by
// code even when the original error was produced via domain.Wrap with a
// fresh underlying error.
func (e *AppError) Is(target error) bool {
	var t *AppError
	if errors.As(target, &t) {
		return e.Code == t.Code
	}
	return false
}

// WithDetail attaches a key/value detail that will be serialized in the
// JSON error response under error.details.
func (e *AppError) WithDetail(key string, value any) *AppError {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

// NewAppError constructs an AppError with a message.
func NewAppError(code string, status int, msg string) *AppError {
	return &AppError{Code: code, Status: status, Err: errors.New(msg)}
}

// Wrap annotates an existing error with a business code and HTTP status.
func Wrap(code string, status int, err error) *AppError {
	return &AppError{Code: code, Status: status, Err: err}
}

// Error code constants. Aligned with openviking/server/error_mapping.py.
// The RESOURCE_/VALIDATION_/etc. flavored codes are the OpenViking-API
// surface; the unprefixed codes (NOT_FOUND, INVALID_ARGUMENT, ...) mirror
// the Python upstream-mapping table for errors propagated from LLM/VL/
// embedder/vikingdb providers.
const (
	CodeResourceNotFound   = "RESOURCE_NOT_FOUND"
	CodeAccountNotFound    = "ACCOUNT_NOT_FOUND"
	CodeUnauthorized       = "UNAUTHORIZED"
	CodeForbidden          = "FORBIDDEN"
	CodeAccountDisabled    = "ACCOUNT_DISABLED"
	CodeConflict           = "CONFLICT"
	CodeValidationFailed   = "VALIDATION_FAILED"
	CodeQuotaExceeded      = "QUOTA_EXCEEDED"
	CodeRateLimited        = "RATE_LIMITED"
	CodePayloadTooLarge    = "PAYLOAD_TOO_LARGE"
	CodeUnsupportedMedia   = "UNSUPPORTED_MEDIA_TYPE"
	CodeParseFailed        = "PARSE_FAILED"
	CodeEmbedFailed        = "EMBED_FAILED"
	CodeRerankFailed       = "RERANK_FAILED"
	CodeVLMFailed          = "VLM_FAILED"
	CodeVectorDBError      = "VECTORDB_ERROR"
	CodeRAGFSError         = "RAGFS_ERROR"
	CodeQueueError         = "QUEUE_ERROR"
	CodeInternalError      = "INTERNAL_ERROR"
	CodeServiceUnavailable = "SERVICE_UNAVAILABLE"
	CodeUnsupported        = "UNSUPPORTED"

	// Upstream-flavored codes mirror the Python error_mapping.py table.
	// They are returned when an upstream provider error (LLM, VL embedder,
	// vikingdb, ragfs backend, OAuth provider) is mapped to an API
	// response. Distinct from the OpenViking-API codes above so callers
	// can distinguish "the resource you asked for is gone" (RESOURCE_NOT_FOUND)
	// from "the upstream returned 404" (NOT_FOUND).
	CodeInvalidArgument     = "INVALID_ARGUMENT"
	CodeUnauthenticated     = "UNAUTHENTICATED"
	CodePermissionDenied    = "PERMISSION_DENIED"
	CodeNotFound            = "NOT_FOUND"
	CodeDeadlineExceeded    = "DEADLINE_EXCEEDED"
	CodeResourceExhausted   = "RESOURCE_EXHAUSTED"
	CodeUnavailable         = "UNAVAILABLE"
	CodeFailedPrecondition  = "FAILED_PRECONDITION"
	CodeInvalidURI          = "INVALID_URI"
	CodeUpstreamError       = "UPSTREAM_ERROR"
	CodeLockConflict        = "LOCK_CONFLICT"
	CodeSSEError            = "SSE_ERROR"
	CodeOAuthError          = "OAUTH_ERROR"
	CodeInvalidGrant        = "INVALID_GRANT"
	CodeInsufficientStorage = "INSUFFICIENT_STORAGE"
	CodeMethodNotAllowed    = "METHOD_NOT_ALLOWED"
)

// Sentinel errors. Use errors.Is(err, domain.ErrNotFound) for cheap matching.
var (
	ErrNotFound         = NewAppError(CodeResourceNotFound, 404, "resource not found")
	ErrAccountNotFound  = NewAppError(CodeAccountNotFound, 404, "account not found")
	ErrUnauthorized     = NewAppError(CodeUnauthorized, 401, "unauthorized")
	ErrForbidden        = NewAppError(CodeForbidden, 403, "forbidden")
	ErrAccountDisabled  = NewAppError(CodeAccountDisabled, 403, "account disabled")
	ErrConflict         = NewAppError(CodeConflict, 409, "conflict")
	ErrValidation       = NewAppError(CodeValidationFailed, 422, "validation failed")
	ErrQuotaExceeded    = NewAppError(CodeQuotaExceeded, 429, "quota exceeded")
	ErrRateLimited      = NewAppError(CodeRateLimited, 429, "rate limited")
	ErrPayloadTooLarge  = NewAppError(CodePayloadTooLarge, 413, "payload too large")
	ErrUnsupportedMedia = NewAppError(CodeUnsupportedMedia, 415, "unsupported media type")
	ErrUnsupported      = NewAppError(CodeUnsupported, 501, "operation not supported")
	ErrParseFailed      = NewAppError(CodeParseFailed, 422, "parse failed")
	ErrEmbedFailed      = NewAppError(CodeEmbedFailed, 502, "embed failed")
	ErrRerankFailed     = NewAppError(CodeRerankFailed, 502, "rerank failed")
	ErrVLMFailed        = NewAppError(CodeVLMFailed, 502, "vlm failed")
	ErrVectorDB         = NewAppError(CodeVectorDBError, 500, "vectordb error")
	ErrRAGFS            = NewAppError(CodeRAGFSError, 500, "ragfs error")
	ErrQueue            = NewAppError(CodeQueueError, 500, "queue error")
	ErrInternal         = NewAppError(CodeInternalError, 500, "internal error")
	ErrUnavailable      = NewAppError(CodeServiceUnavailable, 503, "service unavailable")

	// Upstream-flavored sentinels. MapError returns these (or a fresh
	// *AppError with the same code) when an upstream provider error is
	// translated. Callers can use errors.Is to branch on the category.
	ErrInvalidArgument     = NewAppError(CodeInvalidArgument, 400, "invalid argument")
	ErrUnauthenticated     = NewAppError(CodeUnauthenticated, 401, "unauthenticated")
	ErrPermissionDenied    = NewAppError(CodePermissionDenied, 403, "permission denied")
	ErrUpstreamNotFound    = NewAppError(CodeNotFound, 404, "not found")
	ErrDeadlineExceeded    = NewAppError(CodeDeadlineExceeded, 504, "deadline exceeded")
	ErrResourceExhausted   = NewAppError(CodeResourceExhausted, 429, "resource exhausted")
	ErrUpstreamUnavailable = NewAppError(CodeUnavailable, 503, "unavailable")
	ErrFailedPrecondition  = NewAppError(CodeFailedPrecondition, 412, "failed precondition")
	ErrInvalidURI          = NewAppError(CodeInvalidURI, 400, "invalid uri")
	ErrUpstreamError       = NewAppError(CodeUpstreamError, 502, "upstream error")
	ErrLockConflict        = NewAppError(CodeLockConflict, 423, "lock conflict")
	ErrSSEError            = NewAppError(CodeSSEError, 500, "sse error")
	ErrOAuthError          = NewAppError(CodeOAuthError, 401, "oauth error")
	ErrInvalidGrant        = NewAppError(CodeInvalidGrant, 400, "invalid grant")
	ErrInsufficientStorage = NewAppError(CodeInsufficientStorage, 507, "insufficient storage")
	ErrMethodNotAllowed    = NewAppError(CodeMethodNotAllowed, 405, "method not allowed")
)
