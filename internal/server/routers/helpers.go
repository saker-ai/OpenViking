package routers

import (
	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/ragfs/plugins/memfs"
)

// newMemoryBackend returns a fresh in-memory ragfs backend. It is the
// default backend used by console mount endpoints when no concrete
// filesystem credentials are provided. The indirection keeps the routers
// package decoupled from the memfs plugin so tests can substitute a fake.
func newMemoryBackend(name string) (ragfs.FileSystem, error) {
	return memfs.New(name), nil
}

// abortWithError attaches an *domain.AppError to the request context and
// aborts the chain. The errorMiddleware (in package server) renders the
// structured envelope. Routers must call this rather than c.JSON-with-error
// so the response shape stays consistent.
func abortWithError(c *gin.Context, err *domain.AppError) {
	_ = c.Error(err)
	c.Abort()
}

// okResponse is the success envelope mirroring openviking/server/models.py
// Response(status="ok", result=...). It is the standard shape returned by
// every Python router endpoint so Go clients see a 1:1 field parity.
//
// Routers pass the inner result payload (the data Python would put under
// Response.result) and okResponse wraps it with {"status":"ok","result":...}.
// Optional telemetry/profile fields are omitted when nil to match Python's
// model_dump(exclude_none=True) behaviour.
func okResponse(result any) gin.H {
	return gin.H{
		"status": "ok",
		"result": result,
	}
}

// okResponseWith is like okResponse but also emits the optional telemetry
// and profile fields when non-nil. Routers that collect telemetry should
// prefer this variant so the response shape matches Python's Response model
// end-to-end.
func okResponseWith(result any, telemetry map[string]any, profile []string) gin.H {
	resp := gin.H{
		"status": "ok",
		"result": result,
	}
	if telemetry != nil {
		resp["telemetry"] = telemetry
	}
	if profile != nil {
		resp["profile"] = profile
	}
	return resp
}
