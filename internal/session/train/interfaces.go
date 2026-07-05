// Package train — protocol interfaces for the policy-optimization
// framework.
//
// Mirrors openviking/session/train/interfaces.py: each interface is a
// small role in the train/eval pipeline. Concrete implementations live
// in subdirectories (e.g. internal/session/train/components/...) or in
// operator-supplied packages.

package train

import "context"

// CaseLoader loads case batches for policy optimization. Mirrors the
// Python CaseLoader protocol — batches is an async iterator yielding
// lists of cases. In Go we model this as a Next-style iterator backed
// by a context.
type CaseLoader interface {
	// NextBatch returns the next batch of cases, or io.EOF when
	// exhausted. The returned slice is owned by the caller.
	NextBatch(ctx context.Context) ([]Case, error)
	// Reset repositions the loader at the start of the dataset.
	Reset(ctx context.Context) error
	// Name returns the loader's identifier for logging.
	Name() string
}

// RolloutExecutor executes cases against a policy set and produces
// rollouts. Concrete implementations include LLM-driven single-turn
// and multi-turn executors; the framework only requires the contract.
type RolloutExecutor interface {
	Execute(ctx context.Context, cases []Case, policySet PolicySet, ec ExecutionContext) ([]Rollout, error)
}

// RolloutAnalyzer analyzes a rollout and extracts learning signals
// (rubric evaluation + trajectories + any pre-extracted gradients).
type RolloutAnalyzer interface {
	Analyze(ctx context.Context, rollout Rollout, analysisContext any) (RolloutAnalysis, error)
}

// RolloutEvaluator evaluates a rollout against a rubric. This is a
// sub-step of analysis; some analyzers compose an evaluator.
type RolloutEvaluator interface {
	Evaluate(ctx context.Context, rollout Rollout, evalContext any) (RubricEvaluation, error)
}

// GradientEstimator estimates semantic gradients from rollout analyses.
type GradientEstimator interface {
	Estimate(ctx context.Context, analysis RolloutAnalysis, policySet PolicySet, gradientContext any) ([]PatchSemanticGradient, error)
}

// PolicyOptimizer plans policy-set updates from semantic gradients.
type PolicyOptimizer interface {
	Plan(ctx context.Context, gradients []PatchSemanticGradient, policySet PolicySet, optimizationContext any) (PolicyUpdatePlan, error)
}

// PolicyUpdater applies a policy update plan to a PolicySet.
type PolicyUpdater interface {
	Apply(ctx context.Context, plan PolicyUpdatePlan, policySet PolicySet, applyContext any) (PolicyApplyResult, error)
}

// PolicySnapshotter creates a snapshot identifier for a PolicySet.
type PolicySnapshotter interface {
	Snapshot(ctx context.Context, policySet PolicySet, snapshotContext any) (string, error)
}

// PolicyTrainer trains a policy from rollout batches. This is the
// online/realtime counterpart of one offline pipeline training epoch.
type PolicyTrainer interface {
	TrainRollouts(ctx context.Context, rollouts []Rollout, policySet PolicySet, trainContext any, analyses []RolloutAnalysis) (RolloutTrainingResult, error)
}

// PolicyOptimizationPipeline runs end-to-end policy optimization over
// case batches. The Go OfflinePolicyOptimizationPipeline implements
// this; the interface exists so operators can swap in alternative
// orchestration strategies.
type PolicyOptimizationPipeline interface {
	Train(ctx context.Context, caseLoader CaseLoader, policySet PolicySet, pc PipelineContext) (PipelineResult, error)
	Eval(ctx context.Context, caseLoader CaseLoader, policySet PolicySet, pc PipelineContext) (PipelineEvaluationResult, error)
	TrainFromRollouts(ctx context.Context, rollouts []Rollout, policySet PolicySet, pc PipelineContext) (RolloutTrainingResult, error)
}
