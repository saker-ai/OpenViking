// Package train — context and lifecycle types.
//
// Mirrors openviking/session/train/context.py: PipelineContext,
// ExecutionContext, and PipelineHookDecision.

package train

import "context"

// PipelineHookDecision is a control decision returned by lifecycle
// hooks. When StopTraining is true, the pipeline halts after the
// current epoch.
type PipelineHookDecision struct {
	StopTraining bool
	Reason       string
	Metadata     map[string]any
	Report       map[string]any
}

// PipelineContext bundles per-run configuration for
// OfflinePolicyOptimizationPipeline. Context payloads are intentionally
// opaque (typed as any) so concrete implementations can shape them
// without changing the framework interfaces.
type PipelineContext struct {
	CaseLoadContext    any
	SnapshotContext    any
	AnalysisContext    any
	GradientContext    any
	OptimizationContext any
	ApplyContext       any
	ExecutionMetadata  map[string]any
	MaxEpochs          int
	EvalEachEpochCaseLoader CaseLoader
	EvalTrials         int
	TrainTrials        int
	TrialIndexKey      string
	ReportBuilder      PipelineReportBuilder
	LifecycleHooks     []PipelineLifecycleHook
}

// ExecutionContext is the runtime context passed to RolloutExecutor.
type ExecutionContext struct {
	PolicySnapshotID string
	Metadata         map[string]any
}

// PipelineReportBuilder is an interface for accumulating structured
// reports during a pipeline run. Concrete implementations write to
// files, stdout, or telemetry sinks.
type PipelineReportBuilder interface {
	// RecordEpoch appends one epoch's result to the report.
	RecordEpoch(ctx context.Context, epoch PipelineEpochResult) error
	// RecordEvaluation appends one eval pass to the report.
	RecordEvaluation(ctx context.Context, pass PipelineEvaluationResult) error
	// Build returns the final report payload.
	Build() (map[string]any, error)
}

// PipelineLifecycleHook is a hook called at pipeline milestones. Hooks
// can return a PipelineHookDecision to stop the run early.
type PipelineLifecycleHook interface {
	// OnEpochEnd is called after each epoch. The returned decision can
	// stop the pipeline.
	OnEpochEnd(ctx context.Context, epoch int, result PipelineEpochResult) (PipelineHookDecision, error)
	// OnEvaluationEnd is called after each eval pass.
	OnEvaluationEnd(ctx context.Context, pass PipelineEvaluationResult) (PipelineHookDecision, error)
	// OnTrainEnd is called after the train phase completes.
	OnTrainEnd(ctx context.Context, result PipelineResult) error
	// Name returns the hook's identifier for logging.
	Name() string
}
