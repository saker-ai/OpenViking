// Package train — batch train/eval orchestration.
//
// Mirrors openviking/session/train/batch_runner.py: run_batch_train_eval
// drives a remote benchmark train/eval run using a
// BatchTrainEvalConfig. The Go version is a thin orchestrator: it
// validates config, resolves defaults, then delegates to a
// BatchRunner implementation that talks to the benchmark service.
//
// The framework provides a LocalBatchRunner that wires together the
// OfflinePolicyOptimizationPipeline + ListCaseLoader for in-process
// runs. Operators wanting remote benchmark execution supply a
// RemoteBatchRunner that POSTs to the benchmark service.

package train

import (
	"context"
	"fmt"
	"time"
)

// BatchRunner runs one batch train/eval job. The framework provides
// LocalBatchRunner; operators supply RemoteBatchRunner for offloading
// to a benchmark service.
type BatchRunner interface {
	// Run executes one batch train/eval run and returns a report.
	Run(ctx context.Context, cfg BatchTrainEvalConfig) (BatchTrainEvalReport, error)
}

// RunBatchTrainEval is the top-level entry point. It validates and
// defaults cfg, then delegates to runner. When runner is nil, it
// uses LocalBatchRunner with the supplied pipeline.
//
// This mirrors the Python run_batch_train_eval function, which is the
// CLI entry point invoked by run_batch_train_eval.sh.
func RunBatchTrainEval(ctx context.Context, cfg BatchTrainEvalConfig, runner BatchRunner) (BatchTrainEvalReport, error) {
	if runner == nil {
		return BatchTrainEvalReport{}, fmt.Errorf("train: batch runner is nil")
	}
	cfg = cfg.WithDefaults()
	if err := cfg.Validate(); err != nil {
		return BatchTrainEvalReport{}, fmt.Errorf("train: config: %w", err)
	}
	return runner.Run(ctx, cfg)
}

// LocalBatchRunner runs the batch train/eval pipeline in-process using
// the supplied OfflinePolicyOptimizationPipeline. It does not offload
// to a remote benchmark service; for remote execution, supply a
// RemoteBatchRunner instead.
type LocalBatchRunner struct {
	Pipeline   *OfflinePolicyOptimizationPipeline
	CaseLoader CaseLoader
	PolicySet  PolicySet
}

// Run executes the local pipeline and returns a report. The report's
// OutputPath and EventsPath reflect the cfg fields (the LocalBatchRunner
// does not write to disk itself; operators wanting file output should
// wire a PipelineReportBuilder).
func (r *LocalBatchRunner) Run(ctx context.Context, cfg BatchTrainEvalConfig) (BatchTrainEvalReport, error) {
	if r.Pipeline == nil {
		return BatchTrainEvalReport{}, fmt.Errorf("train: local runner: pipeline is nil")
	}
	if r.CaseLoader == nil {
		return BatchTrainEvalReport{}, fmt.Errorf("train: local runner: case_loader is nil")
	}
	report := BatchTrainEvalReport{
		Domain:       cfg.Domain,
		Dataset:      cfg.Dataset,
		Epochs:       cfg.Epochs,
		Trials:       cfg.Trials,
		OutputPath:   cfg.OutputPath,
		EventsPath:   cfg.EventsPath,
		RunTimestamp: cfg.RunTimestamp,
		StartedAt:    time.Now(),
		Metadata:     map[string]any{},
	}
	pc := PipelineContext{
		MaxEpochs:          cfg.Epochs,
		EvalTrials:         cfg.Trials,
		TrainTrials:        cfg.TrainTrials,
		TrialIndexKey:      "trial",
		ExecutionMetadata:  map[string]any{"domain": cfg.Domain, "dataset": cfg.Dataset},
		LifecycleHooks:     []PipelineLifecycleHook{&NoopLifecycleHook{}},
	}
	result, err := r.Pipeline.Train(ctx, r.CaseLoader, r.PolicySet, pc)
	if err != nil {
		report.FinishedAt = time.Now()
		report.Metadata["error"] = err.Error()
		return report, err
	}
	report.Metadata["epochs_run"] = len(result.Epochs)
	report.Metadata["analyses_count"] = len(result.Analyses)
	report.Metadata["gradients_count"] = len(result.Gradients)
	report.FinishedAt = time.Now()
	return report, nil
}

// NoopLifecycleHook is a no-op hook that lets every run continue.
// Useful as the default hook so pipeline code can iterate
// pc.LifecycleHooks without a nil check.
type NoopLifecycleHook struct{}

// OnEpochEnd returns a no-op decision (continue training).
func (h *NoopLifecycleHook) OnEpochEnd(ctx context.Context, epoch int, result PipelineEpochResult) (PipelineHookDecision, error) {
	return PipelineHookDecision{}, nil
}

// OnEvaluationEnd returns a no-op decision.
func (h *NoopLifecycleHook) OnEvaluationEnd(ctx context.Context, pass PipelineEvaluationResult) (PipelineHookDecision, error) {
	return PipelineHookDecision{}, nil
}

// OnTrainEnd is a no-op.
func (h *NoopLifecycleHook) OnTrainEnd(ctx context.Context, result PipelineResult) error {
	return nil
}

// Name returns the hook's identifier.
func (h *NoopLifecycleHook) Name() string { return "noop" }

// Compile-time assertion.
var _ PipelineLifecycleHook = (*NoopLifecycleHook)(nil)
