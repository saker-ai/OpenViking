package middleware

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// Timeout enforces a per-request deadline. If the wrapped handler chain
// runs past the configured timeout, the middleware responds with
// 504 GATEWAY_TIMEOUT.
//
// The handler runs in a goroutine writing into a buffered ResponseWriter;
// on success the buffer is flushed to the real writer, on timeout a 504
// body is written instead. This avoids concurrent writes to the
// underlying http.ResponseWriter.
func Timeout(timeout time.Duration) gin.HandlerFunc {
	if timeout <= 0 {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)

		buf := &bufferedWriter{header: cloneHeader(c.Writer.Header())}
		orig := c.Writer
		c.Writer = buf

		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Next()
		}()

		select {
		case <-done:
			buf.flush(orig)
		case <-ctx.Done():
			orig.Header().Set("X-Timeout", strconv.Itoa(int(timeout.Milliseconds())))
			orig.WriteHeader(http.StatusGatewayTimeout)
			_, _ = orig.Write([]byte(`{"error":{"code":"GATEWAY_TIMEOUT","message":"request timed out"}}`))
			// Drain the goroutine so it doesn't leak; handler will see
			// ctx.Done() and is expected to return promptly.
			<-done
		}
		// Restore the original writer so downstream "after" phases see
		// the real ResponseWriter, not the spent buffer.
		c.Writer = orig
	}
}

// bufferedWriter captures everything the handler writes without touching
// the underlying ResponseWriter. It is NOT safe for concurrent use; the
// goroutine running the handler is the only writer until flush.
type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
	wrote  bool
}

func cloneHeader(src http.Header) http.Header {
	dst := make(http.Header, len(src))
	for k, vs := range src {
		cp := make([]string, len(vs))
		copy(cp, vs)
		dst[k] = cp
	}
	return dst
}

func (b *bufferedWriter) flush(w gin.ResponseWriter) {
	for k, vs := range b.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	w.WriteHeader(b.status)
	_, _ = w.Write(b.body.Bytes())
}

// Implement gin.ResponseWriter over the buffered writer.

func (b *bufferedWriter) Header() http.Header { return b.header }

func (b *bufferedWriter) Write(data []byte) (int, error) {
	b.wrote = true
	return b.body.Write(data)
}

func (b *bufferedWriter) WriteHeader(code int) {
	if code > 0 {
		b.status = code
		b.wrote = true
	}
}

func (b *bufferedWriter) WriteHeaderNow() {
	b.wrote = true
}

func (b *bufferedWriter) WriteString(s string) (int, error) {
	return b.body.WriteString(s)
}

func (b *bufferedWriter) Written() bool { return b.wrote }

func (b *bufferedWriter) Size() int { return b.body.Len() }

func (b *bufferedWriter) Status() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}

// Hijack — buffered writers cannot be hijacked; satisfy the interface so
// the gin chain does not panic when middleware like SSE probes for it.
func (b *bufferedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errHijackNotSupported
}

func (b *bufferedWriter) CloseNotify() <-chan bool { return nil }

func (b *bufferedWriter) Flush() {}

func (b *bufferedWriter) Pusher() http.Pusher { return nil }

var errHijackNotSupported = errors.New("bufferedWriter: hijack not supported")
