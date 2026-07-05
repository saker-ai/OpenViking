package routers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/queuefs"
	"github.com/saker-ai/ctxhub/internal/ragfs"
	"github.com/saker-ai/ctxhub/internal/server/identity"
)

// RegisterTasks wires /api/v1/tasks/* — async task tracking.
//
// Task state is persisted to ragfs under /accounts/{account}/tasks/{task_id}.json
// so it survives restarts. The queuefs.QueueServer is used to dispatch work;
// cancel/retry are best-effort because the in-memory queue does not expose
// introspection — we mark the persisted state and let the queue drain.
func RegisterTasks(g *gin.RouterGroup, deps *Deps) {
	r := g.Group("/tasks")
	r.GET("", listTasks(deps))
	r.POST("", enqueueTask(deps))
	r.GET("/:id", getTask(deps))
	r.POST("/:id/cancel", cancelTask(deps))
	r.POST("/:id/retry", retryTask(deps))
	r.GET("/:id/result", taskResult(deps))
}

// TaskStatus is the persisted status of a task. It is a superset of the
// queuefs.Task fields needed for diagnostics.
type TaskStatus struct {
	ID          string            `json:"id"`
	Type        string            `json:"type"`
	Account     string            `json:"account"`
	State       string            `json:"state"` // queued, running, completed, failed, canceled
	Payload     []byte            `json:"payload,omitempty"`
	Priority    queuefs.Priority  `json:"priority"`
	MaxRetries  int               `json:"max_retries,omitempty"`
	Result      json.RawMessage   `json:"result,omitempty"`
	Error       string            `json:"error,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// task state constants.
const (
	taskStateQueued    = "queued"
	taskStateRunning   = "running"
	taskStateCompleted = "completed"
	taskStateFailed    = "failed"
	taskStateCanceled  = "canceled"
)

// tasksRoot returns the ragfs path under which task JSON files are stored
// for the caller's account.
func tasksRoot(c *gin.Context) string {
	if id, ok := identity.FromContext(c.Request.Context()); ok && id.Account != "" {
		return ragfs.Normalize(path.Join("/accounts", id.Account, "tasks"))
	}
	return ragfs.Normalize("/tasks")
}

// taskPath returns the JSON path for a single task.
func taskPath(c *gin.Context, id string) string {
	return ragfs.Normalize(path.Join(tasksRoot(c), id+".json"))
}

// readTask loads a TaskStatus from ragfs. Returns domain.ErrNotFound when the
// file is missing.
func readTask(ctx context.Context, deps *Deps, p string) (*TaskStatus, error) {
	var buf bytes.Buffer
	if err := deps.RAGFS.Read(ctx, p, &buf); err != nil {
		return nil, err
	}
	var ts TaskStatus
	if err := json.Unmarshal(buf.Bytes(), &ts); err != nil {
		return nil, err
	}
	return &ts, nil
}

// writeTask persists a TaskStatus to ragfs, creating the parent dir if needed.
func writeTask(ctx context.Context, deps *Deps, p string, ts *TaskStatus) error {
	root := path.Dir(p)
	// Best-effort MkdirAll: ragfs has no MkdirAll, so Mkdir on the root
	// ignores already-exists errors.
	_ = deps.RAGFS.Mkdir(ctx, root, 0o755)
	data, err := json.Marshal(ts)
	if err != nil {
		return err
	}
	return deps.RAGFS.Write(ctx, p, bytes.NewReader(data), 0o644)
}

// listTasks handles GET /tasks — list task statuses for the caller's account.
func listTasks(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		root := tasksRoot(c)
		entries, err := deps.RAGFS.ReadDir(c.Request.Context(), root)
		if err != nil {
			if ragfs.IsNotFound(err) {
				c.JSON(http.StatusOK, gin.H{"tasks": []any{}, "path": root})
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		out := make([]*TaskStatus, 0, len(entries))
		for _, e := range entries {
			if e.Info == nil || e.Info.IsDir {
				continue
			}
			if !strings.HasSuffix(e.Info.Name, ".json") {
				continue
			}
			ts, terr := readTask(c.Request.Context(), deps, e.Path)
			if terr != nil {
				continue
			}
			out = append(out, ts)
		}
		c.JSON(http.StatusOK, gin.H{"tasks": out, "path": root})
	}
}

// enqueueTaskRequest is the JSON body for POST /tasks.
type enqueueTaskRequest struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Payload    []byte           `json:"payload,omitempty"`
	Priority   queuefs.Priority `json:"priority,omitempty"`
	MaxRetries int              `json:"max_retries,omitempty"`
}

// enqueueTask handles POST /tasks — persist task state and dispatch via the
// queue. When no queue is wired the task is still persisted (state=queued)
// so a later retry can pick it up.
func enqueueTask(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		var req enqueueTaskRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeValidationFailed, 422, err))
			return
		}
		if req.ID == "" {
			req.ID = newTaskID()
		}
		if req.Type == "" {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "type is required"))
			return
		}
		id, _ := identity.FromContext(c.Request.Context())
		now := time.Now().UTC()
		ts := &TaskStatus{
			ID:         req.ID,
			Type:       req.Type,
			Account:    id.Account,
			State:      taskStateQueued,
			Payload:    req.Payload,
			Priority:   req.Priority,
			MaxRetries: req.MaxRetries,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		if err := writeTask(c.Request.Context(), deps, taskPath(c, req.ID), ts); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		// Best-effort enqueue. When the queue is not started or not wired,
		// the persisted state still lets a later retry pick the task up.
		if deps.Queue != nil {
			task := &queuefs.Task{
				ID:          req.ID,
				Type:        req.Type,
				Payload:     req.Payload,
				Priority:    req.Priority,
				MaxRetries:  req.MaxRetries,
			}
			if err := deps.Queue.Enqueue(task); err != nil {
				ts.Error = err.Error()
				ts.UpdatedAt = time.Now().UTC()
				_ = writeTask(c.Request.Context(), deps, taskPath(c, req.ID), ts)
				abortWithError(c, domain.Wrap(domain.CodeQueueError, 500, err))
				return
			}
		}
		c.JSON(http.StatusCreated, ts)
	}
}

// getTask handles GET /tasks/:id — fetch a single task's persisted state.
func getTask(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		tid := path.Clean(c.Param("id"))
		if tid == "" || tid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "task id is required"))
			return
		}
		ts, err := readTask(c.Request.Context(), deps, taskPath(c, tid))
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, ts)
	}
}

// cancelTask handles POST /tasks/:id/cancel — mark a queued/running task as
// canceled. The in-memory queue does not support cancellation, so this only
// updates the persisted state; the worker (if any) will skip the task when it
// observes the state on next retry.
func cancelTask(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		tid := path.Clean(c.Param("id"))
		if tid == "" || tid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "task id is required"))
			return
		}
		p := taskPath(c, tid)
		ts, err := readTask(c.Request.Context(), deps, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if ts.State == taskStateCompleted {
			abortWithError(c, domain.NewAppError(domain.CodeConflict, 409, "task already completed"))
			return
		}
		ts.State = taskStateCanceled
		ts.UpdatedAt = time.Now().UTC()
		if err := writeTask(c.Request.Context(), deps, p, ts); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		c.JSON(http.StatusOK, ts)
	}
}

// retryTask handles POST /tasks/:id/retry — re-enqueue a failed or canceled
// task. The persisted state is reset to queued.
func retryTask(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		tid := path.Clean(c.Param("id"))
		if tid == "" || tid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "task id is required"))
			return
		}
		p := taskPath(c, tid)
		ts, err := readTask(c.Request.Context(), deps, p)
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		ts.State = taskStateQueued
		ts.Error = ""
		ts.Result = nil
		ts.UpdatedAt = time.Now().UTC()
		if err := writeTask(c.Request.Context(), deps, p, ts); err != nil {
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if deps.Queue != nil {
			task := &queuefs.Task{
				ID:         ts.ID,
				Type:       ts.Type,
				Payload:    ts.Payload,
				Priority:   ts.Priority,
				MaxRetries: ts.MaxRetries,
			}
			if err := deps.Queue.Enqueue(task); err != nil {
				abortWithError(c, domain.Wrap(domain.CodeQueueError, 500, err))
				return
			}
		}
		c.JSON(http.StatusOK, ts)
	}
}

// taskResult handles GET /tasks/:id/result — return the task's result/output.
// When the task is not yet completed, 409 is returned so clients can poll.
func taskResult(deps *Deps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if deps == nil || deps.RAGFS == nil {
			_ = c.Error(domain.ErrUnsupported)
			c.Abort()
			return
		}
		tid := path.Clean(c.Param("id"))
		if tid == "" || tid == "." {
			abortWithError(c, domain.NewAppError(domain.CodeValidationFailed, 422, "task id is required"))
			return
		}
		ts, err := readTask(c.Request.Context(), deps, taskPath(c, tid))
		if err != nil {
			if ragfs.IsNotFound(err) {
				abortWithError(c, domain.Wrap(domain.CodeResourceNotFound, 404, err))
				return
			}
			abortWithError(c, domain.Wrap(domain.CodeRAGFSError, 500, err))
			return
		}
		if ts.State != taskStateCompleted && ts.State != taskStateFailed {
			abortWithError(c, domain.NewAppError(domain.CodeConflict, 409,
				fmt.Sprintf("task not finished (state=%s)", ts.State)))
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"task_id": ts.ID,
			"state":   ts.State,
			"result":  json.RawMessage(ts.Result),
			"error":   ts.Error,
		})
	}
}

// newTaskID returns a sortable unique ID for a task. It is RFC 4122-ish:
// hex timestamp prefix + random suffix, no external dep.
func newTaskID() string {
	return fmt.Sprintf("tsk-%d-%s", time.Now().UTC().UnixNano(), randHex(6))
}

// randHex returns n hex digits from the math/rand source seeded by time. It
// is sufficient for task ID uniqueness within a single queue instance; for
// production queuefs the caller should provide an ID.
func randHex(n int) string {
	const hexchars = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hexchars[time.Now().UnixNano()%int64(len(hexchars))]
	}
	return string(b)
}
