package queuefs

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// dagPayload is the JSON envelope the scheduler wraps around the user's
// payload when enqueuing a task. It lets the wrapped handler recover the
// instance ID and node ID to call MarkDone / MarkFailed.
type dagPayload struct {
	InstanceID string `json:"instance_id"`
	NodeID     string `json:"node_id"`
	Payload    []byte `json:"payload"`
}

// DAGScheduler orchestrates topological execution of tasks with
// dependencies. It wraps a QueueServer: when a handler registered via
// RegisterHandler completes, the wrapper updates the instance and enqueues
// any dependents whose dependencies are now all satisfied.
//
// The scheduler is transport-agnostic. It works with any QueueServer, but
// dead-letter integration (MarkFailed on permanent failure) requires the
// backend to implement DeadLetterNotifier. The memory backend does; the
// redis backend does not in P5 (asynq's archive acts as DLQ).
type DAGScheduler struct {
	server QueueServer

	mu        sync.Mutex
	instances map[string]*dagInstance
}

type dagInstance struct {
	id         string
	spec       map[string]*Task    // nodeID -> task
	deps       map[string][]string // nodeID -> dependency IDs
	dependents map[string][]string // nodeID -> dependent node IDs
	done       map[string]*TaskResult
	failed     map[string]error
	status     DAGStatus
	doneCh     chan struct{}
}

// NewDAGScheduler wraps srv with DAG orchestration. If srv implements
// DeadLetterNotifier, the scheduler installs a hook that marks failed DAG
// nodes when their retries are exhausted.
func NewDAGScheduler(srv QueueServer) *DAGScheduler {
	s := &DAGScheduler{
		server:    srv,
		instances: make(map[string]*dagInstance),
	}
	if dl, ok := srv.(DeadLetterNotifier); ok {
		dl.SetDeadLetterHook(s.deadLetterHook)
	}
	return s
}

// Submit validates the DAG, creates an in-memory instance, and enqueues all
// root tasks (those with no Dependencies). Returns the instance ID (caller
// can pass it to Wait) or an error if validation fails.
func (s *DAGScheduler) Submit(ctx context.Context, tasks []*Task) (string, error) {
	if _, err := topologicalSort(tasks); err != nil {
		return "", err
	}

	inst := &dagInstance{
		id:         uuid.NewString(),
		spec:       make(map[string]*Task, len(tasks)),
		deps:       make(map[string][]string, len(tasks)),
		dependents: make(map[string][]string, len(tasks)),
		done:       make(map[string]*TaskResult, len(tasks)),
		failed:     make(map[string]error, len(tasks)),
		status:     DAGStatusRunning,
		doneCh:     make(chan struct{}),
	}
	for _, t := range tasks {
		tCopy := *t
		inst.spec[t.ID] = &tCopy
		inst.deps[t.ID] = append([]string(nil), t.Dependencies...)
		for _, d := range t.Dependencies {
			inst.dependents[d] = append(inst.dependents[d], t.ID)
		}
	}

	s.mu.Lock()
	s.instances[inst.id] = inst
	s.mu.Unlock()

	// Enqueue roots. If a single root fails to enqueue, mark the whole
	// instance partial_failed so Wait does not block forever.
	for _, t := range tasks {
		if len(t.Dependencies) == 0 {
			if err := s.enqueueTask(inst.id, t); err != nil {
				s.MarkFailed(inst.id, t.ID, err)
				return inst.id, fmt.Errorf("enqueue root %s: %w", t.ID, err)
			}
		}
	}
	return inst.id, nil
}

// RegisterHandler binds a HandlerFunc to a task type on the underlying
// QueueServer. The handler is wrapped so that on success it calls MarkDone
// (which enqueues ready dependents), and on failure it returns the error
// (letting the backend retry; the dead-letter hook calls MarkFailed when
// retries are exhausted).
func (s *DAGScheduler) RegisterHandler(taskType string, handler HandlerFunc) {
	wrapped := func(ctx context.Context, task *Task) (*TaskResult, error) {
		var dp dagPayload
		if err := json.Unmarshal(task.Payload, &dp); err != nil {
			return nil, fmt.Errorf("queuefs: invalid dag payload: %w", err)
		}
		inner := *task
		inner.ID = dp.NodeID
		inner.Payload = dp.Payload
		result, err := handler(ctx, &inner)
		if err != nil {
			// Backend will retry. If retries exhaust, the dead-letter hook
			// (set on memory backend) calls MarkFailed.
			return nil, err
		}
		s.MarkDone(dp.InstanceID, dp.NodeID, result)
		return result, nil
	}
	s.server.RegisterHandler(taskType, wrapped)
}

// MarkDone records a successful result and enqueues any dependents whose
// dependencies are now all satisfied. If this was the last pending node,
// the instance transitions to Completed and Wait unblocks.
func (s *DAGScheduler) MarkDone(instID, nodeID string, result *TaskResult) {
	s.mu.Lock()
	inst, ok := s.instances[instID]
	if !ok {
		s.mu.Unlock()
		return
	}
	inst.done[nodeID] = result

	ready := make([]*Task, 0)
	for _, dep := range inst.dependents[nodeID] {
		if s.allDepsDoneLocked(inst, dep) {
			if t, ok := inst.spec[dep]; ok {
				ready = append(ready, t)
			}
		}
	}

	if len(inst.done) == len(inst.spec) && inst.status == DAGStatusRunning {
		inst.status = DAGStatusCompleted
		close(inst.doneCh)
	}
	s.mu.Unlock()

	for _, t := range ready {
		_ = s.enqueueTask(instID, t)
	}
}

// MarkFailed records a permanent failure and transitions the instance to
// PartialFailed (if still Running), unblocking Wait. Dependents of the
// failed node are not enqueued.
func (s *DAGScheduler) MarkFailed(instID, nodeID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[instID]
	if !ok {
		return
	}
	inst.failed[nodeID] = err
	if inst.status == DAGStatusRunning {
		inst.status = DAGStatusPartialFailed
		close(inst.doneCh)
	}
}

// Wait blocks until the DAG instance reaches a terminal state (Completed or
// PartialFailed) or ctx is cancelled. Returns the terminal status.
func (s *DAGScheduler) Wait(ctx context.Context, instID string) (DAGStatus, error) {
	s.mu.Lock()
	inst, ok := s.instances[instID]
	s.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("queuefs: unknown dag instance %s", instID)
	}
	select {
	case <-inst.doneCh:
		return inst.status, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// Status returns the current DAGStatus of an instance.
func (s *DAGScheduler) Status(instID string) (DAGStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inst, ok := s.instances[instID]
	if !ok {
		return "", fmt.Errorf("queuefs: unknown dag instance %s", instID)
	}
	return inst.status, nil
}

// enqueueTask wraps the task payload in a dagPayload and enqueues it on the
// underlying server.
func (s *DAGScheduler) enqueueTask(instID string, t *Task) error {
	dp := dagPayload{InstanceID: instID, NodeID: t.ID, Payload: t.Payload}
	data, err := json.Marshal(dp)
	if err != nil {
		return fmt.Errorf("queuefs: marshal dag payload: %w", err)
	}
	wrapped := &Task{
		ID:           t.ID,
		Type:         t.Type,
		Payload:      data,
		Dependencies: t.Dependencies,
		Priority:     t.Priority,
		MaxRetries:   t.MaxRetries,
	}
	return s.server.Enqueue(wrapped)
}

// deadLetterHook is installed on backends implementing DeadLetterNotifier.
// It decodes the dagPayload from the dead-lettered task and calls
// MarkFailed so the DAG instance does not hang in Running.
func (s *DAGScheduler) deadLetterHook(task *Task, err error) {
	var dp dagPayload
	if json.Unmarshal(task.Payload, &dp) != nil {
		return
	}
	s.MarkFailed(dp.InstanceID, dp.NodeID, err)
}

// allDepsDoneLocked reports whether every dependency of nodeID is recorded
// in inst.done. Caller must hold inst.mu (s.mu).
func (s *DAGScheduler) allDepsDoneLocked(inst *dagInstance, nodeID string) bool {
	for _, d := range inst.deps[nodeID] {
		if _, ok := inst.done[d]; !ok {
			return false
		}
	}
	return true
}
