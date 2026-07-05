// Package queuefs implements the OpenViking task queue and DAG scheduler.
//
// # DAG Execution Model
//
// A DAG (directed acyclic graph) of Tasks is submitted to DAGScheduler.Submit.
// The scheduler validates the graph (returning ErrCycleDetected on cycles),
// creates an in-memory instance, and enqueues all root tasks (those with no
// Dependencies) on the underlying QueueServer. Each enqueued task carries a
// JSON-encoded dagPayload containing the instance ID, node ID, and the user's
// original payload.
//
// Handlers registered via DAGScheduler.RegisterHandler are wrapped with
// bookkeeping:
//
//   - On success the wrapper calls MarkDone, which records the result and
//     enqueues any dependents whose dependencies are now all satisfied.
//   - On failure the wrapper returns the error; the backend retries per
//     Task.MaxRetries with exponential backoff. When retries are exhausted
//     the backend's dead-letter hook calls MarkFailed, which marks the DAG
//     instance partial_failed and unblocks Wait.
//
// Parallelism: independent tasks (siblings in the DAG) are enqueued
// simultaneously and execute concurrently up to the backend's concurrency
// limit. Dependencies serialize only what must be serialized.
//
// # Backends
//
// The memory backend is in-process, channel-based, with three priority queues
// (critical, default, low), exponential backoff retry, and a dead-letter
// slice. It is the default for tests and dev.
//
// The redis backend is backed by github.com/hibiken/asynq. Tasks are routed
// to asynq queues "critical"/"default"/"low" by Task.Priority; retry and
// dead-lettering are delegated to asynq.
//
// # Node Types
//
// The seven DAG node types (parse, embed, upsert, extract_abstract,
// extract_overview, rerank, commit) are defined as constants below; their
// default priority and retry counts follow design doc 7.4.
package queuefs
