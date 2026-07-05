// Package train — default offline policy optimization pipeline.
//
// Mirrors openviking/session/train/pipeline.py: OfflinePolicyOptimizationPipeline
// wires the protocol interfaces together. Train updates the policy
// set from case rollouts; Eval only executes and analyzes rollouts
// without estimating gradients or writing policy files. Benchmark
// runners should explicitly compose them, e.g. eval(test) → train(train)
// → eval(test).

package train

import (
	"context"
	"fmt"
)

// OfflinePolicyOptimizationPipeline is the default composable
// train/eval pipeline. It does not implement rollout execution, LLM
// analysis, gradient estimation, optimization, or file updates itself;
// it composes operator-supplied implementations of those interfaces.
type OfflinePolicyOptimizationPipeline struct {
	Snapshotter       PolicySnapshotter
	RolloutExecutor   RolloutExecutor
	RolloutAnalyzer   RolloutAnalyzer
	GradientEstimator GradientEstimator
	PolicyOptimizer   PolicyOptimizer
	PolicyUpdater     PolicyUpdater
	PolicyTrainer     PolicyTrainer
	// Engine is the shared analyze→estimate→plan→apply core. When
	// nil, the pipeline constructs one from the analyzer/estimator/
	// optimizer/updater above on first use.
	Engine *PolicyTrainingEngine
}

// NewOfflinePipeline constructs a pipeline with the given components.
// If policyTrainer is nil, the pipeline uses the engine's
// AnalyzeEstimatePlanApply for TrainFromRollouts.
func NewOfflinePipeline(
	snapshotter PolicySnapshotter,
	rolloutExecutor RolloutExecutor,
	rolloutAnalyzer RolloutAnalyzer,
	gradientEstimator GradientEstimator,
	policyOptimizer PolicyOptimizer,
	policyUpdater PolicyUpdater,
	policyTrainer PolicyTrainer,
) *OfflinePolicyOptimizationPipeline {
	return &OfflinePolicyOptimizationPipeline{
		Snapshotter:       snapshotter,
		RolloutExecutor:   rolloutExecutor,
		RolloutAnalyzer:   rolloutAnalyzer,
		GradientEstimator: gradientEstimator,
		PolicyOptimizer:   policyOptimizer,
		PolicyUpdater:     policyUpdater,
		PolicyTrainer:     policyTrainer,
	}
}

// engine returns the shared engine, constructing one if not set.
func (p *OfflinePolicyOptimizationPipeline) engine() *PolicyTrainingEngine {
	if p.Engine != nil {
		return p.Engine
	}
	p.Engine = &PolicyTrainingEngine{
		RolloutAnalyzer:   p.RolloutAnalyzer,
		GradientEstimator: p.GradientEstimator,
		PolicyOptimizer:   p.PolicyOptimizer,
		PolicyUpdater:     p.PolicyUpdater,
	}
	return p.Engine
}

// Train runs the train pipeline over case batches. For each epoch:
//
//  1. Snapshot the current policy set.
//  2. Load the next batch of cases.
//  3. Execute rollouts against the snapshot.
//  4. Analyze → estimate → plan → apply (via the shared engine).
//  5. Run any lifecycle hooks.
//
// EvalEachEpochCaseLoader, when set, runs an eval pass after each
// epoch. The final result aggregates all epochs.
func (p *OfflinePolicyOptimizationPipeline) Train(
	ctx context.Context,
	caseLoader CaseLoader,
	policySet PolicySet,
	pc PipelineContext,
) (PipelineResult, error) {
	if err := p.validate(); err != nil {
		return PipelineResult{}, err
	}
	if caseLoader == nil {
		return PipelineResult{}, fmt.Errorf("train: case_loader is nil")
	}
	maxEpochs := pc.MaxEpochs
	if maxEpochs <= 0 {
		maxEpochs = 1
	}
	engine := p.engine()
	result := PipelineResult{Metadata: map[string]any{}}
	for epoch := 1; epoch <= maxEpochs; epoch++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		// Snapshot before executing rollouts so all cases in this
		// epoch run against the same policy version.
		snapshotID, err := p.Snapshotter.Snapshot(ctx, policySet, pc.SnapshotContext)
		if err != nil {
			return result, fmt.Errorf("train: snapshot epoch %d: %w", epoch, err)
		}
		if err := caseLoader.Reset(ctx); err != nil {
			return result, fmt.Errorf("train: reset case loader epoch %d: %w", epoch, err)
		}
		var epochRollouts []Rollout
		for {
			batch, err := caseLoader.NextBatch(ctx)
			if err != nil {
				if isEOF(err) {
					break
				}
				return result, fmt.Errorf("train: load cases epoch %d: %w", epoch, err)
			}
			rollouts, err := p.RolloutExecutor.Execute(ctx, batch, policySet, ExecutionContext{
				PolicySnapshotID: snapshotID,
				Metadata:         pc.ExecutionMetadata,
			})
			if err != nil {
				return result, fmt.Errorf("train: execute epoch %d: %w", epoch, err)
			}
			epochRollouts = append(epochRollouts, rollouts...)
		}
		analyses, gradients, plan, applyResult, err := engine.AnalyzeEstimatePlanApply(ctx, epochRollouts, policySet, pc)
		if err != nil {
			return result, fmt.Errorf("train: engine epoch %d: %w", epoch, err)
		}
		epochResult := PipelineEpochResult{
			Epoch:             epoch,
			Analyses:          analyses,
			Gradients:         gradients,
			Plan:              plan,
			ApplyResult:       applyResult,
			PolicySnapshotIDs: []string{snapshotID},
			Metadata:          map[string]any{},
		}
		result.Epochs = append(result.Epochs, epochResult)
		result.Analyses = append(result.Analyses, analyses...)
		result.Gradients = append(result.Gradients, gradients...)
		result.Plan = plan
		result.ApplyResult = applyResult
		// Lifecycle hooks.
		for _, hook := range pc.LifecycleHooks {
			decision, err := hook.OnEpochEnd(ctx, epoch, epochResult)
			if err != nil {
				return result, fmt.Errorf("train: hook %s epoch %d: %w", hook.Name(), epoch, err)
			}
			if decision.StopTraining {
				result.Metadata["stop_reason"] = decision.Reason
				// Train-end hooks still fire on early stop (mirrors
				// the Python lifecycle: a stop decision ends the
				// epoch loop but the train-end milestone is still
				// observed for cleanup/report flush).
				for _, h := range pc.LifecycleHooks {
					if err := h.OnTrainEnd(ctx, result); err != nil {
						return result, fmt.Errorf("train: hook %s train-end: %w", h.Name(), err)
					}
				}
				return result, nil
			}
		}
		// Optional eval-each-epoch pass.
		if pc.EvalEachEpochCaseLoader != nil {
			evalResult, err := p.Eval(ctx, pc.EvalEachEpochCaseLoader, policySet, pc)
			if err != nil {
				return result, fmt.Errorf("train: eval-each-epoch %d: %w", epoch, err)
			}
			result.EvaluationPasses = append(result.EvaluationPasses, evalResult)
		}
	}
	// Final train-end hooks.
	for _, hook := range pc.LifecycleHooks {
		if err := hook.OnTrainEnd(ctx, result); err != nil {
			return result, fmt.Errorf("train: hook %s train-end: %w", hook.Name(), err)
		}
	}
	return result, nil
}

// Eval runs an evaluation-only pass. Cases are executed against a
// fresh snapshot and analyzed; no gradients are estimated and no
// policy files are written.
func (p *OfflinePolicyOptimizationPipeline) Eval(
	ctx context.Context,
	caseLoader CaseLoader,
	policySet PolicySet,
	pc PipelineContext,
) (PipelineEvaluationResult, error) {
	if err := p.validate(); err != nil {
		return PipelineEvaluationResult{}, err
	}
	if caseLoader == nil {
		return PipelineEvaluationResult{}, fmt.Errorf("train: case_loader is nil")
	}
	if p.RolloutAnalyzer == nil {
		return PipelineEvaluationResult{}, fmt.Errorf("train: rollout_analyzer is nil")
	}
	snapshotID, err := p.Snapshotter.Snapshot(ctx, policySet, pc.SnapshotContext)
	if err != nil {
		return PipelineEvaluationResult{}, fmt.Errorf("train: snapshot: %w", err)
	}
	if err := caseLoader.Reset(ctx); err != nil {
		return PipelineEvaluationResult{}, fmt.Errorf("train: reset: %w", err)
	}
	var allRollouts []Rollout
	for {
		batch, err := caseLoader.NextBatch(ctx)
		if err != nil {
			if isEOF(err) {
				break
			}
			return PipelineEvaluationResult{}, fmt.Errorf("train: load: %w", err)
		}
		rollouts, err := p.RolloutExecutor.Execute(ctx, batch, policySet, ExecutionContext{
			PolicySnapshotID: snapshotID,
			Metadata:         pc.ExecutionMetadata,
		})
		if err != nil {
			return PipelineEvaluationResult{}, fmt.Errorf("train: execute: %w", err)
		}
		allRollouts = append(allRollouts, rollouts...)
	}
	analyses := make([]RolloutAnalysis, len(allRollouts))
	for i, r := range allRollouts {
		a, err := p.RolloutAnalyzer.Analyze(ctx, r, pc.AnalysisContext)
		if err != nil {
			return PipelineEvaluationResult{}, fmt.Errorf("train: analyze %d: %w", i, err)
		}
		analyses[i] = a
	}
	pass := PipelineEvaluationResult{
		Epoch:             0, // eval-only pass has no epoch number
		Analyses:          analyses,
		PolicySnapshotIDs: []string{snapshotID},
		Metadata:          map[string]any{},
	}
	for _, hook := range pc.LifecycleHooks {
		decision, err := hook.OnEvaluationEnd(ctx, pass)
		if err != nil {
			return pass, fmt.Errorf("train: hook %s eval-end: %w", hook.Name(), err)
		}
		if decision.StopTraining {
			pass.Metadata["stop_reason"] = decision.Reason
			return pass, nil
		}
	}
	return pass, nil
}

// TrainFromRollouts trains directly from externally produced rollouts.
// This is the online/realtime counterpart of one offline training
// epoch. The caller owns rollout execution; the framework owns
// analysis, gradient estimation, policy planning, and policy update.
//
// The pipeline supports two composition modes:
//   - When PolicyTrainer is set, delegate to it (operator-supplied
//     training strategy, e.g. a streaming trainer keyed by submitter).
//   - Otherwise, use the shared engine's AnalyzeEstimatePlanApply
//     (the same path as offline epochs).
func (p *OfflinePolicyOptimizationPipeline) TrainFromRollouts(
	ctx context.Context,
	rollouts []Rollout,
	policySet PolicySet,
	pc PipelineContext,
) (RolloutTrainingResult, error) {
	if err := p.validate(); err != nil {
		return RolloutTrainingResult{}, err
	}
	if p.PolicyTrainer != nil {
		return p.PolicyTrainer.TrainRollouts(ctx, rollouts, policySet, pc.OptimizationContext, nil)
	}
	engine := p.engine()
	analyses, gradients, plan, applyResult, err := engine.AnalyzeEstimatePlanApply(ctx, rollouts, policySet, pc)
	if err != nil {
		return RolloutTrainingResult{}, err
	}
	return RolloutTrainingResult{
		Analyses:    analyses,
		Gradients:   gradients,
		Plan:        plan,
		ApplyResult: applyResult,
		Metadata:    map[string]any{},
	}, nil
}

// validate returns an error if required components are missing.
func (p *OfflinePolicyOptimizationPipeline) validate() error {
	if p.Snapshotter == nil {
		return fmt.Errorf("train: snapshotter is nil")
	}
	if p.RolloutExecutor == nil {
		return fmt.Errorf("train: rollout_executor is nil")
	}
	if p.RolloutAnalyzer == nil {
		return fmt.Errorf("train: rollout_analyzer is nil")
	}
	if p.GradientEstimator == nil {
		return fmt.Errorf("train: gradient_estimator is nil")
	}
	if p.PolicyOptimizer == nil {
		return fmt.Errorf("train: policy_optimizer is nil")
	}
	if p.PolicyUpdater == nil {
		return fmt.Errorf("train: policy_updater is nil")
	}
	return nil
}

// Compile-time assertion that the pipeline satisfies the interface.
var _ PolicyOptimizationPipeline = (*OfflinePolicyOptimizationPipeline)(nil)

// isEOF reports whether err is an end-of-iteration sentinel. The
// framework uses context.Canceled or a custom EOF sentinel; the
// concrete CaseLoader implementations decide which.
func isEOF(err error) bool {
	if err == nil {
		return false
	}
	if err == ErrEndOfBatches {
		return true
	}
	return false
}

// ErrEndOfBatches is the sentinel returned by CaseLoader.NextBatch
// when the dataset is exhausted.
var ErrEndOfBatches = fmt.Errorf("train: end of batches")
