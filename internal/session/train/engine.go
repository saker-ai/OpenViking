// Package train — shared training engine.
//
// Mirrors openviking/session/train/engine.py: PolicyTrainingEngine
// implements the analyze → estimate → plan → apply loop. It is the
// shared core used by both the offline pipeline and the online
// (streaming) trainer.
//
// The Go implementation is concurrency-bounded by a semaphore; the
// Python version uses asyncio.gather. The analyze and estimate phases
// run their inputs concurrently because each call is independent.

package train

import (
	"context"
	"fmt"
	"sync"
)

// PolicyTrainingEngine is the shared implementation of
// analyze→estimate→plan→apply.
type PolicyTrainingEngine struct {
	RolloutAnalyzer   RolloutAnalyzer
	GradientEstimator GradientEstimator
	PolicyOptimizer   PolicyOptimizer
	PolicyUpdater     PolicyUpdater
	// Concurrency limits the parallelism of analyze and estimate
	// calls. When 0, defaults to runtime.NumCPU(). Values <0 are
	// clamped to 1.
	Concurrency int
}

// AnalyzeEstimatePlanApply runs the full loop over a batch of rollouts.
// Returns the analyses, gradients, plan, and apply result.
func (e *PolicyTrainingEngine) AnalyzeEstimatePlanApply(
	ctx context.Context,
	rollouts []Rollout,
	policySet PolicySet,
	pc PipelineContext,
) ([]RolloutAnalysis, []PatchSemanticGradient, PolicyUpdatePlan, PolicyApplyResult, error) {
	analyses, err := e.AnalyzeRollouts(ctx, rollouts, pc)
	if err != nil {
		return nil, nil, PolicyUpdatePlan{}, PolicyApplyResult{}, err
	}
	gradients, err := e.EstimateGradients(ctx, analyses, policySet, pc)
	if err != nil {
		return analyses, nil, PolicyUpdatePlan{}, PolicyApplyResult{}, err
	}
	plan, applyResult, err := e.PlanAndApply(ctx, gradients, policySet, pc)
	if err != nil {
		return analyses, gradients, PolicyUpdatePlan{}, PolicyApplyResult{}, err
	}
	return analyses, gradients, plan, applyResult, nil
}

// AnalyzeRollouts runs the analyzer concurrently over each rollout.
// Preserves rollout order in the returned slice.
func (e *PolicyTrainingEngine) AnalyzeRollouts(ctx context.Context, rollouts []Rollout, pc PipelineContext) ([]RolloutAnalysis, error) {
	if e.RolloutAnalyzer == nil {
		return nil, fmt.Errorf("train: rollout_analyzer is nil")
	}
	analyses := make([]RolloutAnalysis, len(rollouts))
	if len(rollouts) == 0 {
		return analyses, nil
	}
	sem := makeSemaphore(e.Concurrency, len(rollouts))
	var wg sync.WaitGroup
	errs := make([]error, len(rollouts))
	for i, r := range rollouts {
		wg.Add(1)
		go func(i int, r Rollout) {
			defer wg.Done()
			sem.acquire(ctx)
			defer sem.release()
			a, err := e.RolloutAnalyzer.Analyze(ctx, r, pc.AnalysisContext)
			if err != nil {
				errs[i] = err
				return
			}
			analyses[i] = a
		}(i, r)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return analyses, nil
}

// EstimateGradients runs the estimator concurrently over each analysis.
// Returns a flat slice of all gradients produced.
func (e *PolicyTrainingEngine) EstimateGradients(ctx context.Context, analyses []RolloutAnalysis, policySet PolicySet, pc PipelineContext) ([]PatchSemanticGradient, error) {
	if e.GradientEstimator == nil {
		return nil, fmt.Errorf("train: gradient_estimator is nil")
	}
	if len(analyses) == 0 {
		return nil, nil
	}
	batches := make([][]PatchSemanticGradient, len(analyses))
	sem := makeSemaphore(e.Concurrency, len(analyses))
	var wg sync.WaitGroup
	errs := make([]error, len(analyses))
	for i, a := range analyses {
		wg.Add(1)
		go func(i int, a RolloutAnalysis) {
			defer wg.Done()
			sem.acquire(ctx)
			defer sem.release()
			gs, err := e.GradientEstimator.Estimate(ctx, a, policySet, pc.GradientContext)
			if err != nil {
				errs[i] = err
				return
			}
			batches[i] = gs
		}(i, a)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	var out []PatchSemanticGradient
	for _, b := range batches {
		out = append(out, b...)
	}
	return out, nil
}

// PlanAndApply runs the optimizer and updater serially under the
// policy set's lock. The lock+reload+plan+apply sequence mirrors the
// Python async-with policy_set.lock() pattern; the Go version delegates
// locking to the caller's PolicySet implementation (the framework
// does not enforce a specific lock provider).
func (e *PolicyTrainingEngine) PlanAndApply(ctx context.Context, gradients []PatchSemanticGradient, policySet PolicySet, pc PipelineContext) (PolicyUpdatePlan, PolicyApplyResult, error) {
	if e.PolicyOptimizer == nil {
		return PolicyUpdatePlan{}, PolicyApplyResult{}, fmt.Errorf("train: policy_optimizer is nil")
	}
	if e.PolicyUpdater == nil {
		return PolicyUpdatePlan{}, PolicyApplyResult{}, fmt.Errorf("train: policy_updater is nil")
	}
	plan, err := e.PolicyOptimizer.Plan(ctx, gradients, policySet, pc.OptimizationContext)
	if err != nil {
		return PolicyUpdatePlan{}, PolicyApplyResult{}, fmt.Errorf("train: plan: %w", err)
	}
	applyCtx := pc.ApplyContext
	if applyCtx == nil {
		applyCtx = policySet.RequestContext
	}
	applyResult, err := e.PolicyUpdater.Apply(ctx, plan, policySet, applyCtx)
	if err != nil {
		return plan, PolicyApplyResult{}, fmt.Errorf("train: apply: %w", err)
	}
	return plan, applyResult, nil
}

// semaphore is a counting semaphore using a buffered channel. The
// acquire/release methods are context-aware (acquire returns early
// when ctx is canceled).
type semaphore struct {
	ch chan struct{}
}

func makeSemaphore(concurrency, total int) *semaphore {
	n := concurrency
	if n <= 0 {
		// runtime.NumCPU() is the natural default for CPU-bound work,
		// but the train pipeline is I/O-bound (LLM calls). Cap at 8 to
		// avoid hammering downstream services when Concurrency is unset.
		n = 8
	}
	if n > total {
		n = total
	}
	if n < 1 {
		n = 1
	}
	return &semaphore{ch: make(chan struct{}, n)}
}

func (s *semaphore) acquire(ctx context.Context) {
	select {
	case s.ch <- struct{}{}:
	case <-ctx.Done():
	}
}

func (s *semaphore) release() {
	select {
	case <-s.ch:
	default:
	}
}
