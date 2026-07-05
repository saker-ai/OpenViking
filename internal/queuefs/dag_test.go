package queuefs

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDAGTopologicalOrder verifies that a linear chain A -> B -> C executes
// in dependency order (A before B before C).
func TestDAGTopologicalOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string

	srv := NewMemoryServer(WithConcurrency(4))
	sched := NewDAGScheduler(srv)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()

	recorder := func(_ context.Context, task *Task) (*TaskResult, error) {
		mu.Lock()
		order = append(order, task.ID)
		mu.Unlock()
		return &TaskResult{}, nil
	}
	for _, nt := range AllNodeTypes() {
		sched.RegisterHandler(nt, recorder)
	}

	tasks := []*Task{
		{ID: "A", Type: NodeParse, Priority: PriorityCritical, MaxRetries: 0},
		{ID: "B", Type: NodeEmbed, Priority: PriorityDefault, MaxRetries: 0, Dependencies: []string{"A"}},
		{ID: "C", Type: NodeUpsert, Priority: PriorityDefault, MaxRetries: 0, Dependencies: []string{"B"}},
	}
	id, err := sched.Submit(context.Background(), tasks)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := sched.Wait(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, DAGStatusCompleted, status)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"A", "B", "C"}, order, "tasks should execute in topological order")
}

// TestDAGCycleDetection verifies that Submit rejects a cyclic graph.
func TestDAGCycleDetection(t *testing.T) {
	srv := NewMemoryServer()
	sched := NewDAGScheduler(srv)

	tasks := []*Task{
		{ID: "A", Type: NodeParse, Dependencies: []string{"B"}},
		{ID: "B", Type: NodeEmbed, Dependencies: []string{"A"}},
	}
	_, err := sched.Submit(context.Background(), tasks)
	require.ErrorIs(t, err, ErrCycleDetected)
}

// TestDAGUnknownDependency verifies Submit rejects a missing dependency.
func TestDAGUnknownDependency(t *testing.T) {
	srv := NewMemoryServer()
	sched := NewDAGScheduler(srv)

	tasks := []*Task{
		{ID: "A", Type: NodeParse, Dependencies: []string{"missing"}},
	}
	_, err := sched.Submit(context.Background(), tasks)
	require.ErrorIs(t, err, ErrUnknownDependency)
}

func TestDAGDuplicateTaskID(t *testing.T) {
	srv := NewMemoryServer()
	sched := NewDAGScheduler(srv)

	tasks := []*Task{
		{ID: "A", Type: NodeParse},
		{ID: "A", Type: NodeEmbed},
	}
	_, err := sched.Submit(context.Background(), tasks)
	require.ErrorIs(t, err, ErrDuplicateTaskID)
}

func TestDAGEmptyRejected(t *testing.T) {
	srv := NewMemoryServer()
	sched := NewDAGScheduler(srv)
	_, err := sched.Submit(context.Background(), nil)
	require.ErrorIs(t, err, ErrEmptyDAG)
}

// TestDAGParallelWherePossible verifies that independent sibling tasks
// (B and C, both depending only on A) execute concurrently.
func TestDAGParallelWherePossible(t *testing.T) {
	var inFlight int32
	var maxInFlight int32

	srv := NewMemoryServer(WithConcurrency(8))
	sched := NewDAGScheduler(srv)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()

	track := func(_ context.Context, task *Task) (*TaskResult, error) {
		n := atomic.AddInt32(&inFlight, 1)
		for {
			m := atomic.LoadInt32(&maxInFlight)
			if n <= m || atomic.CompareAndSwapInt32(&maxInFlight, m, n) {
				break
			}
		}
		// Hold long enough for the sibling to start concurrently.
		time.Sleep(40 * time.Millisecond)
		atomic.AddInt32(&inFlight, -1)
		return &TaskResult{}, nil
	}
	for _, nt := range AllNodeTypes() {
		sched.RegisterHandler(nt, track)
	}

	tasks := []*Task{
		{ID: "A", Type: NodeParse, MaxRetries: 0},
		{ID: "B", Type: NodeEmbed, MaxRetries: 0, Dependencies: []string{"A"}},
		{ID: "C", Type: NodeExtractAbstract, MaxRetries: 0, Dependencies: []string{"A"}},
		{ID: "D", Type: NodeCommit, MaxRetries: 0, Dependencies: []string{"B", "C"}},
	}
	id, err := sched.Submit(context.Background(), tasks)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := sched.Wait(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, DAGStatusCompleted, status)

	require.GreaterOrEqual(t, atomic.LoadInt32(&maxInFlight), int32(2),
		"B and C should run in parallel; max in-flight was %d", atomic.LoadInt32(&maxInFlight))
}

// TestDAGSevenNodeTypesEndToEnd registers a fake handler for each of the
// seven node types and runs a full DAG: parse -> embed -> upsert -> rerank
// -> commit, with parse -> extract_abstract -> extract_overview feeding
// into rerank as well.
func TestDAGSevenNodeTypesEndToEnd(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]bool)

	srv := NewMemoryServer(WithConcurrency(4))
	sched := NewDAGScheduler(srv)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()

	for _, nt := range AllNodeTypes() {
		nt := nt
		sched.RegisterHandler(nt, func(ctx context.Context, task *Task) (*TaskResult, error) {
			mu.Lock()
			seen[nt] = true
			mu.Unlock()
			return &TaskResult{Output: []byte(nt)}, nil
		})
	}

	tasks := []*Task{
		{ID: "parse", Type: NodeParse, Priority: DefaultPriority(NodeParse), MaxRetries: DefaultRetries(NodeParse)},
		{ID: "embed", Type: NodeEmbed, Priority: DefaultPriority(NodeEmbed), MaxRetries: DefaultRetries(NodeEmbed), Dependencies: []string{"parse"}},
		{ID: "upsert", Type: NodeUpsert, Priority: DefaultPriority(NodeUpsert), MaxRetries: DefaultRetries(NodeUpsert), Dependencies: []string{"embed"}},
		{ID: "extract_abstract", Type: NodeExtractAbstract, Priority: DefaultPriority(NodeExtractAbstract), MaxRetries: DefaultRetries(NodeExtractAbstract), Dependencies: []string{"parse"}},
		{ID: "extract_overview", Type: NodeExtractOverview, Priority: DefaultPriority(NodeExtractOverview), MaxRetries: DefaultRetries(NodeExtractOverview), Dependencies: []string{"extract_abstract"}},
		{ID: "rerank", Type: NodeRerank, Priority: DefaultPriority(NodeRerank), MaxRetries: DefaultRetries(NodeRerank), Dependencies: []string{"upsert", "extract_overview"}},
		{ID: "commit", Type: NodeCommit, Priority: DefaultPriority(NodeCommit), MaxRetries: DefaultRetries(NodeCommit), Dependencies: []string{"rerank"}},
	}
	id, err := sched.Submit(context.Background(), tasks)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := sched.Wait(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, DAGStatusCompleted, status)

	mu.Lock()
	defer mu.Unlock()
	for _, nt := range AllNodeTypes() {
		assert.True(t, seen[nt], "node type %s should have been invoked", nt)
	}
}

// TestDAGFailureMarksPartialFailed verifies that when a node exhausts its
// retries, the DAG instance is marked partial_failed and Wait unblocks.
func TestDAGFailureMarksPartialFailed(t *testing.T) {
	srv := NewMemoryServer(WithConcurrency(2))
	sched := NewDAGScheduler(srv)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()

	sched.RegisterHandler(NodeParse, func(ctx context.Context, task *Task) (*TaskResult, error) {
		return &TaskResult{}, nil
	})
	sched.RegisterHandler(NodeEmbed, func(ctx context.Context, task *Task) (*TaskResult, error) {
		return nil, errors.New("permanent failure")
	})

	tasks := []*Task{
		{ID: "parse", Type: NodeParse, MaxRetries: 0},
		{ID: "embed", Type: NodeEmbed, MaxRetries: 1, Dependencies: []string{"parse"}},
		{ID: "upsert", Type: NodeUpsert, MaxRetries: 0, Dependencies: []string{"embed"}},
	}
	id, err := sched.Submit(context.Background(), tasks)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	status, err := sched.Wait(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, DAGStatusPartialFailed, status)
}

// TestDAGWaitUnknownInstance verifies Wait returns an error for unknown IDs.
func TestDAGWaitUnknownInstance(t *testing.T) {
	srv := NewMemoryServer()
	sched := NewDAGScheduler(srv)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := sched.Wait(ctx, "nonexistent")
	assert.Error(t, err)
}

// TestDAGStatusAfterCompletion verifies Status reports the terminal state.
func TestDAGStatusAfterCompletion(t *testing.T) {
	srv := NewMemoryServer(WithConcurrency(2))
	sched := NewDAGScheduler(srv)
	require.NoError(t, srv.Start(context.Background()))
	defer srv.Shutdown()

	sched.RegisterHandler(NodeParse, func(context.Context, *Task) (*TaskResult, error) {
		return &TaskResult{}, nil
	})

	id, err := sched.Submit(context.Background(), []*Task{
		{ID: "parse", Type: NodeParse, MaxRetries: 0},
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = sched.Wait(ctx, id)
	require.NoError(t, err)

	got, err := sched.Status(id)
	require.NoError(t, err)
	assert.Equal(t, DAGStatusCompleted, got)
}

// TestDefaultPriorityAndRetries verifies the design-doc defaults.
func TestDefaultPriorityAndRetries(t *testing.T) {
	assert.Equal(t, PriorityCritical, DefaultPriority(NodeParse))
	assert.Equal(t, PriorityDefault, DefaultPriority(NodeEmbed))
	assert.Equal(t, PriorityDefault, DefaultPriority(NodeUpsert))
	assert.Equal(t, PriorityLow, DefaultPriority(NodeExtractAbstract))
	assert.Equal(t, PriorityLow, DefaultPriority(NodeExtractOverview))
	assert.Equal(t, PriorityDefault, DefaultPriority(NodeRerank))
	assert.Equal(t, PriorityDefault, DefaultPriority(NodeCommit))

	assert.Equal(t, 3, DefaultRetries(NodeParse))
	assert.Equal(t, 5, DefaultRetries(NodeEmbed))
	assert.Equal(t, 5, DefaultRetries(NodeUpsert))
	assert.Equal(t, 3, DefaultRetries(NodeExtractAbstract))
	assert.Equal(t, 3, DefaultRetries(NodeExtractOverview))
	assert.Equal(t, 5, DefaultRetries(NodeRerank))
	assert.Equal(t, 3, DefaultRetries(NodeCommit))
}

func TestAllNodeTypesHasSeven(t *testing.T) {
	assert.Len(t, AllNodeTypes(), 7)
}
