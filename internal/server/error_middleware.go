package server

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// errorResponse is the JSON body returned for any AppError. It mirrors the
// Python error_mapping.py envelope: { "error": { "code", "message", "details" } }.
type errorResponse struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

// errorMiddleware runs AFTER handlers (c.Next is called first). It inspects
// c.Errors and renders the first AppError it finds as a structured JSON
// response with the matching HTTP status code. Non-AppError errors are
// routed through MapError so upstream/AGFS/Lock/SSE/OAuth failures are
// rendered with the correct 4xx/5xx code instead of a generic 500; only
// truly unmapped errors fall through to 500 INTERNAL_ERROR.
//
// Idempotent: when the response has already been written (by a deeper
// errorMiddleware registered on the same group, or by a handler that
// rendered directly), this middleware skips re-rendering. Without this
// guard, registering errorMiddleware at both the app and the /api/v1
// group level would write the error body twice — which breaks SDK
// envelope parsing (two concatenated JSON objects can't be unmarshalled).
func errorMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if c.Writer.Written() {
			return
		}
		if len(c.Errors) == 0 {
			return
		}
		first := c.Errors[0]
		var appErr *domain.AppError
		if errors.As(first.Err, &appErr) {
			renderAppError(c, appErr)
			return
		}
		// Non-AppError: consult MapError for a category-specific status.
		status, code, message := MapError(first.Err)
		if code != domain.CodeInternalError {
			slog.Error("mapped upstream error",
				"request_id", c.GetString("request_id"),
				"code", code,
				"status", status,
				"err", first.Err.Error())
			c.JSON(status, errorResponse{
				Error: errorBody{
					Code:    code,
					Message: message,
				},
			})
			return
		}
		slog.Error("unhandled error",
			"request_id", c.GetString("request_id"),
			"err", first.Err.Error())
		c.JSON(http.StatusInternalServerError, errorResponse{
			Error: errorBody{
				Code:    domain.CodeInternalError,
				Message: "internal error",
			},
		})
	}
}

func renderAppError(c *gin.Context, e *domain.AppError) {
	status := e.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	c.JSON(status, errorResponse{
		Error: errorBody{
			Code:    e.Code,
			Message: errorMessage(e),
			Details: e.Details,
		},
	})
}

func errorMessage(e *domain.AppError) string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return e.Code
}

// abortWithError is the helper handlers use to attach an AppError to the
// request context and short-circuit the chain.
func abortWithError(c *gin.Context, err *domain.AppError) {
	_ = c.Error(err)
	c.Abort()
}
