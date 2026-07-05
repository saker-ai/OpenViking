package queuefs

import (
	"context"
	"errors"
	"time"
)

// Priority selects one of the three priority queues. Higher values are
// processed first. The mapping to asynq queues is:
//
//	PriorityCritical -> "critical"
//	PriorityDefault  -> "default"
//	PriorityLow      -> "low"
type Priority int

const (
	// PriorityLow is the lowest priority, used for background understanding
	// tasks (extract_abstract, extract_overview).
	PriorityLow Priority = iota
	// PriorityDefault is the default priority for embed/upsert/rerank/commit.
	PriorityDefault
	// PriorityCritical is the highest priority, used for parse (interactive
	// path) and any user-facing task.
	PriorityCritical
)

// QueueName returns the asynq/memory queue name for a priority.
func (p Priority) QueueName() string {
	switch p {
	case PriorityCritical:
		return "critical"
	case PriorityLow:
		return "low"
	default:
		return "default"
	}
}

// Task is the unit of work enqueued into queuefs. It is transport-agnostic:
// the memory and redis backends both operate on Task values.
type Task struct {
	// ID uniquely identifies this task within a DAG instance. It must be
	// non-empty and unique across the instance. Asynq uses it as TaskID for
	// deduplication.
	ID string

	// Type is the task type, typically one of the Node* constants (e.g.
	// NodeParse). It routes the task to the registered handler.
	Type string

	// Payload is the opaque handler input. For DAG-submitted tasks it is the
	// user's original payload; the scheduler wraps it in a dagPayload before
	// enqueueing.
	Payload []byte

	// Dependencies lists the IDs of tasks that must complete successfully
	// before this task is enqueued. Empty means the task is a root.
	Dependencies []string

	// Priority selects the queue. Zero is PriorityLow; callers should set
	// this explicitly via DefaultPriority(nodeType) when unsure.
	Priority Priority

	// MaxRetries is the number of retries after the initial attempt. Zero
	// means no retries (the task is dead-lettered on first failure). Total
	// attempts = MaxRetries + 1.
	MaxRetries int

	// Account is the per-account isolation key. Tasks enqueued with
	// different Account values are invisible to each other's Claim calls.
	// Empty is allowed and treated as the literal "default" namespace.
	// Used by the SQLite semantic layer (SemanticStore); ignored by the
	// memory and redis backends.
	Account string

	// DelayUntil is the visible-after timestamp. The task is not eligible
	// for Claim until now >= DelayUntil. Zero means immediately visible.
	// Used by the SQLite semantic layer.
	DelayUntil time.Time

	// Attempts is the current attempt count (0 = never tried). Bumped by
	// SemanticStore.Abandon on each failure. Read by Claim to surface the
	// current attempt number to the handler.
	Attempts int

	// LastError is the error message from the most recent failed attempt.
	// Cleared on success. Surface for diagnostics; not used for control
	// flow.
	LastError string
}

// TaskResult is returned by a handler on success. Output is opaque;
// Metadata is for diagnostics and is not used for control flow.
type TaskResult struct {
	Output   []byte
	Metadata map[string]string
}

// HandlerFunc processes a single Task. It is called by the backend's worker
// goroutines. A non-nil error triggers retry up to Task.MaxRetries, after
// which the task is dead-lettered and the dead-letter hook (if any) fires.
type HandlerFunc func(ctx context.Context, task *Task) (*TaskResult, error)

// QueueServer is the backend-agnostic interface every queue implementation
// satisfies. It is a superset of server.QueueServer (Start + Shutdown), so
// any queuefs.QueueServer satisfies the minimal interface in internal/server.
type QueueServer interface {
	// Start launches worker goroutines (memory) or the asynq server (redis).
	// It returns nil once workers are running; it does not block.
	Start(ctx context.Context) error

	// Shutdown stops workers gracefully, draining in-flight tasks. It is
	// idempotent.
	Shutdown() error

	// Enqueue adds a task to a queue. The task's Priority selects the queue.
	// Returns an error if the server is not started or the task is invalid.
	Enqueue(task *Task) error

	// RegisterHandler binds a HandlerFunc to a task type. Multiple calls for
	// the same type replace the prior handler. Must be callable before Start.
	RegisterHandler(taskType string, handler HandlerFunc)
}

// DeadLetterNotifier is an optional interface implemented by backends that
// notify on permanent task failure (retries exhausted). The DAG scheduler
// uses it to mark failed DAG instances.
type DeadLetterNotifier interface {
	SetDeadLetterHook(hook func(*Task, error))
}

// ErrLeaseNotFound is returned by SemanticStore.Renew / Complete / Abandon
// when the supplied lease ID does not match a currently-leased task. This
// usually means the lease expired (RecoverStale re-queued the task) or was
// already completed/abandoned.
var ErrLeaseNotFound = errors.New("queuefs: lease not found")

// Lease is returned by SemanticStore.Claim. The caller must either Complete
// the lease (task is done) or Abandon it (retry / dead-letter). If neither
// happens before ExpiresAt, RecoverStale will re-queue the task for another
// worker — at-least-once delivery.
type Lease struct {
	// ID is the lease identifier. Pass it to Renew / Complete / Abandon.
	ID string

	// TaskID is the ID of the claimed task.
	TaskID string

	// Account is the account the task was enqueued under.
	Account string

	// Task is the claimed task, with Attempts and LastError populated.
	Task *Task

	// ExpiresAt is when the lease expires unless renewed. Renew extends
	// this by the requested TTL.
	ExpiresAt time.Time
}

// TaskRecord is the admin view of a queued task, including lease state and
// enqueue time. Returned by SemanticStore.List.
type TaskRecord struct {
	Task

	// Status is "pending" or "leased".
	Status string

	// EnqueuedAt is when the task was inserted (RFC3339Nano).
	EnqueuedAt time.Time

	// LeaseID is the current lease ID (empty if status is "pending").
	LeaseID string

	// LeaseExpiresAt is when the current lease expires (zero if pending).
	LeaseExpiresAt time.Time
}

// DeadLetter is a permanently failed task in the dead-letter queue.
// Returned by SemanticStore.DeadLetters.
type DeadLetter struct {
	Task

	// DeadAt is when the task was moved to the dead-letter queue.
	DeadAt time.Time
}
