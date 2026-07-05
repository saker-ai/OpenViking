package queuefs

import (
	"errors"
	"fmt"
	"sort"
)

// DAGStatus is the runtime state of a DAG instance.
type DAGStatus string

const (
	// DAGStatusPending means the instance has been created but root tasks
	// have not yet been enqueued. (Transient; Submit transitions to Running.)
	DAGStatusPending DAGStatus = "pending"

	// DAGStatusRunning means at least one task is in flight or queued.
	DAGStatusRunning DAGStatus = "running"

	// DAGStatusCompleted means every node completed successfully.
	DAGStatusCompleted DAGStatus = "completed"

	// DAGStatusPartialFailed means at least one node exhausted its retries.
	// Dependents of the failed node are not enqueued.
	DAGStatusPartialFailed DAGStatus = "partial_failed"
)

// ErrCycleDetected is returned by Submit when the dependency graph contains
// a cycle, making topological ordering impossible.
var ErrCycleDetected = errors.New("queuefs: dag contains a cycle")

// ErrUnknownDependency is returned by Submit when a task lists a dependency
// ID that is not present in the submitted task set.
var ErrUnknownDependency = errors.New("queuefs: unknown dependency")

// ErrDuplicateTaskID is returned by Submit when two tasks share an ID.
var ErrDuplicateTaskID = errors.New("queuefs: duplicate task id")

// ErrEmptyDAG is returned by Submit when the task list is empty.
var ErrEmptyDAG = errors.New("queuefs: empty dag")

// topologicalSort returns node IDs in dependency order using Kahn's
// algorithm. It returns ErrCycleDetected if a cycle exists,
// ErrUnknownDependency if a task references a missing dependency, and
// ErrDuplicateTaskID if two tasks share an ID. The returned order is
// deterministic: among ready nodes, IDs are processed in lexicographic
// order.
func topologicalSort(tasks []*Task) ([]string, error) {
	if len(tasks) == 0 {
		return nil, ErrEmptyDAG
	}

	nodes := make(map[string]*Task, len(tasks))
	dependents := make(map[string][]string, len(tasks)) // depID -> node IDs that depend on it
	indegree := make(map[string]int, len(tasks))

	for _, t := range tasks {
		if t == nil {
			return nil, fmt.Errorf("queuefs: nil task in dag")
		}
		if _, exists := nodes[t.ID]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateTaskID, t.ID)
		}
		nodes[t.ID] = t
		indegree[t.ID] = 0
	}

	for _, t := range tasks {
		for _, d := range t.Dependencies {
			if _, ok := nodes[d]; !ok {
				return nil, fmt.Errorf("%w: %s references missing dependency %s",
					ErrUnknownDependency, t.ID, d)
			}
			dependents[d] = append(dependents[d], t.ID)
			indegree[t.ID]++
		}
	}

	// Kahn's algorithm. Collect ready nodes (indegree 0), sort for
	// determinism, then drain.
	ready := make([]string, 0, len(tasks))
	for id, deg := range indegree {
		if deg == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)

	order := make([]string, 0, len(tasks))
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		order = append(order, n)
		next := append([]string(nil), dependents[n]...)
		sort.Strings(next)
		for _, dep := range next {
			indegree[dep]--
			if indegree[dep] == 0 {
				// Insert maintaining sorted order for determinism.
				ready = append(ready, dep)
				sort.Strings(ready)
			}
		}
	}

	if len(order) != len(tasks) {
		return nil, ErrCycleDetected
	}
	return order, nil
}
