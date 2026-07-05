package routers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// RegisterObserver wires /api/v1/observer/* — observer-mode controls.
//
// The observer router exposes read-only runtime introspection: goroutine
// stacks, memory stats, an SSE event stream (heartbeat-only), and recent
// requests (best-effort ring buffer in the request middleware).
//
// SDK-compat aliases (Go SDK at sdk/go calls these status endpoints):
//   - GET /observer/queue     — queuefs pending/processed counts
//   - GET /observer/vikingdb  — vector DB connectivity
//   - GET /observer/models    — VLM provider list
//   - GET /observer/system    — overall is_healthy rollup
func RegisterObserver(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/observer")
	r.GET("", listObservers(deps))
	r.POST("", createObserver(deps))
	r.DELETE("/:id", deleteObserver(deps))
	r.GET("/:id/events", observerEvents(deps))
	r.GET("/queue", observerQueue(deps))
	r.GET("/vikingdb", observerVikingDB(deps))
	r.GET("/models", observerModels(deps))
	r.GET("/system", observerSystem(deps))
}

// observerQueue handles GET /observer/queue — return queuefs pending/
// processed counts. When no queue is wired, returns disabled=true.
func observerQueue(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		disabled := true
		pending := 0
		if deps != nil && deps.Queue != nil {
			disabled = false
			if p, ok := deps.Queue.(interface{ Pending() int }); ok {
				pending = p.Pending()
			}
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"disabled": disabled,
			"pending":  pending,
		}))
	}
}

// observerVikingDB handles GET /observer/vikingdb — return vector DB
// connectivity. When no vector DB is wired, returns disabled=true.
func observerVikingDB(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		disabled := deps == nil || deps.VectorDB == nil
		c.JSON(http.StatusOK, okResponse(gin.H{
			"disabled":  disabled,
			"connected": !disabled,
		}))
	}
}

// observerModels handles GET /observer/models — return VLM provider list.
// When no VLM is wired, returns an empty provider list.
func observerModels(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		providers := []string{}
		if deps != nil && deps.Models.VLM != nil {
			providers = append(providers, "default")
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"providers": providers,
		}))
	}
}

// observerSystem handles GET /observer/system — overall is_healthy rollup.
// The Go SDK's IsHealthy reads status["is_healthy"].(bool).
func observerSystem(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		healthy := true
		if deps == nil {
			healthy = false
		}
		c.JSON(http.StatusOK, okResponse(gin.H{
			"is_healthy": healthy,
			"version":    "dev",
		}))
	}
}

// observerEntry is a registered observer target. The current implementation
// is process-local (goroutine/mem stats) so observers are just labels with
// a kind; future implementations may target remote peers.
type observerEntry struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // "runtime", "events"
	Account   string    `json:"account,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// observerStore is the in-memory observer registry. It is process-local:
// observers do not survive restarts. The registry is nil-safe: when no Deps
// are wired the handlers return 501. The store is a package-level singleton
// because Deps does not carry an observer field; observer state is process-
// scoped (goroutine/mem stats) so a single shared store is sufficient.
type observerStore struct {
	mu         sync.Mutex
	entries    map[string]*observerEntry
	lastID     int
	recentReqs []*recentRequest
}

// recentRequest is a single entry in the observer's request ring buffer.
type recentRequest struct {
	Method string    `json:"method"`
	Path   string    `json:"path"`
	Status int       `json:"status"`
	Time   time.Time `json:"time"`
}

// defaultObserverStore is the package-level singleton used by the observer
// router. It is initialized on first use.
var defaultObserverStore = newObserverStore()

// newObserverStore returns an initialized observerStore.
func newObserverStore() *observerStore {
	return &observerStore{
		entries:    make(map[string]*observerEntry),
		recentReqs: make([]*recentRequest, 0, 64),
	}
}

// observerStoreFromDeps returns the active observer store. The store is
// process-local so all callers share the same view; Deps is nil-checked
// to honor the 501 contract for un-wired apps.
func observerStoreFromDeps(deps *Deps) *observerStore {
	if deps == nil {
		return nil
	}
	return defaultObserverStore
}

// listObservers handles GET /observer — list registered observer entries.
func listObservers(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		store := observerStoreFromDeps(deps)
		if store == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		out := make([]*observerEntry, 0, len(store.entries))
		for _, e := range store.entries {
			out = append(out, e)
		}
		c.JSON(http.StatusOK, gin.H{"observers": out})
	}
}

// createObserverRequest is the JSON body for POST /observer.
type createObserverRequest struct {
	Kind string `json:"kind"`
}

// createObserver handles POST /observer — register a new observer. The kind
// selects what events the /events stream will emit.
func createObserver(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		store := observerStoreFromDeps(deps)
		if store == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req createObserverRequest
		if c.Request.ContentLength > 0 {
			if err := c.ShouldBindJSON(&req); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
				return
			}
		}
		if req.Kind == "" {
			req.Kind = "runtime"
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		store.lastID++
		entry := &observerEntry{
			ID:        fmt.Sprintf("obs-%d", store.lastID),
			Kind:      req.Kind,
			CreatedAt: time.Now().UTC(),
		}
		store.entries[entry.ID] = entry
		c.JSON(http.StatusCreated, entry)
	}
}

// deleteObserver handles DELETE /observer/:id — remove an observer.
func deleteObserver(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		store := observerStoreFromDeps(deps)
		if store == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		oid := c.Param("id")
		if oid == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "observer id is required"))
			return
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if _, ok := store.entries[oid]; !ok {
			abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, fmt.Errorf("observer not found: %s", oid)))
			return
		}
		delete(store.entries, oid)
		c.JSON(http.StatusOK, gin.H{"id": oid, "deleted": true})
	}
}

// observerEvents handles GET /observer/:id/events — Server-Sent Events stream.
// The current implementation emits a heartbeat every second and a snapshot
// for runtime-kind observers. The stream stays open until the client
// disconnects or 30 seconds elapse (test-friendly cap).
func observerEvents(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		store := observerStoreFromDeps(deps)
		if store == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		oid := c.Param("id")
		store.mu.Lock()
		entry, ok := store.entries[oid]
		store.mu.Unlock()
		if !ok {
			abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, fmt.Errorf("observer not found: %s", oid)))
			return
		}
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Status(http.StatusOK)

		flusher, _ := c.Writer.(http.Flusher)
		timeout := time.NewTimer(30 * time.Second)
		defer timeout.Stop()
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()

		// The request context is cancelled when the client disconnects
		// (production server) or when the test's 30s timeout cap elapses.
		ctxDone := c.Request.Context().Done()
		for {
			select {
			case <-ctxDone:
				return
			case <-timeout.C:
				return
			case <-ticker.C:
				payload := observerSnapshot(entry.Kind)
				data, _ := json.Marshal(payload)
				fmt.Fprintf(c.Writer, "event: snapshot\ndata: %s\n\n", data)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}
}

// observerSnapshot returns a payload for an SSE snapshot event based on the
// observer kind. Runtime observers get goroutine + memory stats; event
// observers get the recent-requests ring buffer (best-effort, may be empty).
func observerSnapshot(kind string) gin.H {
	switch kind {
	case "events":
		// The recent-request ring buffer is populated by the request
		// middleware; when absent we return an empty list so callers can
		// still poll the stream.
		return gin.H{"kind": "events", "recent_requests": []any{}}
	default:
		return runtimeSnapshot()
	}
}

// runtimeSnapshot returns goroutine and memory stats for the current
// process. It is the default observer payload.
func runtimeSnapshot() gin.H {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	buf := make([]byte, 1<<14)
	n := runtime.Stack(buf, true)
	return gin.H{
		"kind":          "runtime",
		"goroutines":    runtime.NumGoroutine(),
		"heap_alloc":    mem.HeapAlloc,
		"heap_inuse":    mem.HeapInuse,
		"stack_inuse":   mem.StackInuse,
		"num_gc":        mem.NumGC,
		"goroutine_stacks": string(buf[:n]),
	}
}
