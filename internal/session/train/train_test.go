package train

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubExecutor is a RolloutExecutor that returns canned rollouts. Each
// case produces one rollout whose messages are the case name.
type stubExecutor struct {
	mu       sync.Mutex
	calls    int
	failOn   int // when >0, fail on the Nth Execute call
	execErr  error
}

func (e *stubExecutor) Execute(ctx context.Context, cases []Case, policySet PolicySet, ec ExecutionContext) ([]Rollout, error) {
	e.mu.Lock()
	e.calls++
	n := e.calls
	e.mu.Unlock()
	if e.failOn > 0 && n == e.failOn {
		return nil, e.execErr
	}
	out := make([]Rollout, len(cases))
	for i, c := range cases {
		out[i] = Rollout{
			Case:             &c,
			Messages:         []Message{{Role: "assistant", Content: c.Name}},
			PolicySnapshotID: ec.PolicySnapshotID,
		}
	}
	return out, nil
}

// stubAnalyzer is a RolloutAnalyzer that returns a passing evaluation
// with the case name as evidence. It can be configured to fail.
type stubAnalyzer struct {
	mu       sync.Mutex
	calls    int
	failOn   int
	err      error
}

func (a *stubAnalyzer) Analyze(ctx context.Context, rollout Rollout, _ any) (RolloutAnalysis, error) {
	a.mu.Lock()
	a.calls++
	n := a.calls
	a.mu.Unlock()
	if a.failOn > 0 && n == a.failOn {
		return RolloutAnalysis{}, a.err
	}
	return RolloutAnalysis{
		Evaluation: RubricEvaluation{
			Passed: true,
			Score:  1.0,
			CriterionResults: []CriterionResult{
				{CriterionName: "smoke", Passed: true, Score: 1.0},
			},
		},
		Trajectories: []Trajectory{
			{Name: rollout.Case.Name, Outcome: TrajectoryOutcomeSuccess},
		},
	}, nil
}

// stubEstimator is a GradientEstimator that returns one gradient per
// analysis, targeting the analysis's first trajectory.
type stubEstimator struct {
	mu    sync.Mutex
	calls int
}

func (e *stubEstimator) Estimate(ctx context.Context, analysis RolloutAnalysis, _ PolicySet, _ any) ([]PatchSemanticGradient, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	if len(analysis.Trajectories) == 0 {
		return nil, nil
	}
	t := analysis.Trajectories[0]
	return []PatchSemanticGradient{{
		AfterFile: MemoryFile{
			URI:        "memory://exp/" + t.Name,
			MemoryType: "experiences",
			ExtraFields: map[string]any{
				"experience_name": t.Name,
			},
		},
		Rationale:  "stub gradient for " + t.Name,
		Confidence: 0.5,
	}}, nil
}

// stubOptimizer is a PolicyOptimizer that produces one upsert item per
// gradient.
type stubOptimizer struct{}

func (o *stubOptimizer) Plan(ctx context.Context, gradients []PatchSemanticGradient, _ PolicySet, _ any) (PolicyUpdatePlan, error) {
	items := make([]PolicyPlanItem, len(gradients))
	for i, g := range gradients {
		items[i] = PolicyPlanItem{
			Kind:       PolicyPlanItemKindUpsert,
			MemoryType: "experiences",
			TargetName: g.TargetName(),
			TargetURI:  g.TargetURI(),
		}
	}
	return PolicyUpdatePlan{Items: items}, nil
}

// snapshotStub is a PolicySnapshotter that returns sequential IDs.
type snapshotStub struct {
	mu    sync.Mutex
	calls int
}

func (s *snapshotStub) Snapshot(ctx context.Context, _ PolicySet, _ any) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return fmt.Sprintf("snap-%d", s.calls), nil
}

// countingHook counts hook calls; can be configured to stop the run.
type countingHook struct {
	mu        sync.Mutex
	epochEnds int
	evalEnds  int
	trainEnds int
	stopAt    int // when >0, stop after the Nth epoch end
}

func (h *countingHook) OnEpochEnd(ctx context.Context, epoch int, _ PipelineEpochResult) (PipelineHookDecision, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.epochEnds++
	if h.stopAt > 0 && epoch >= h.stopAt {
		return PipelineHookDecision{StopTraining: true, Reason: "test stop"}, nil
	}
	return PipelineHookDecision{}, nil
}

func (h *countingHook) OnEvaluationEnd(ctx context.Context, _ PipelineEvaluationResult) (PipelineHookDecision, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.evalEnds++
	return PipelineHookDecision{}, nil
}

func (h *countingHook) OnTrainEnd(ctx context.Context, _ PipelineResult) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trainEnds++
	return nil
}

func (h *countingHook) Name() string { return "counting" }

// makeTestPipeline wires together a pipeline with all stub components.
func makeTestPipeline() *OfflinePolicyOptimizationPipeline {
	return NewOfflinePipeline(
		&snapshotStub{},
		&stubExecutor{},
		&stubAnalyzer{},
		&stubEstimator{},
		&stubOptimizer{},
		NewDryRunUpdater(),
		nil,
	)
}

// makeTestCases returns N cases with simple rubrics.
func makeTestCases(n int) []Case {
	out := make([]Case, n)
	for i := 0; i < n; i++ {
		out[i] = Case{
			Name:          fmt.Sprintf("case-%d", i),
			TaskSignature: fmt.Sprintf("task-%d", i),
			Rubric: Rubric{
				Name:        "smoke",
				Description: "smoke test",
				Criteria: []RubricCriterion{
					{Name: "smoke", Description: "passes smoke", Required: true, Weight: 1.0},
				},
			},
		}
	}
	return out
}

// TestBatchTrainEvalConfig_Validate verifies Validate catches bad
// fields.
func TestBatchTrainEvalConfig_Validate(t *testing.T) {
	cases := []struct {
		name string
		cfg  BatchTrainEvalConfig
		want string
	}{
		{"empty dataset", BatchTrainEvalConfig{Domain: "d"}, "dataset"},
		{"empty domain", BatchTrainEvalConfig{Dataset: "d"}, "domain"},
		{"negative epochs", BatchTrainEvalConfig{Domain: "d", Dataset: "d", Epochs: -1}, "epochs"},
		{"zero trials", BatchTrainEvalConfig{Domain: "d", Dataset: "d", Concurrency: 1, TrainTrials: 1, Trials: 0}, "trials"},
		{"valid", BatchTrainEvalConfig{Domain: "d", Dataset: "d", Epochs: 1, Trials: 1, Concurrency: 1, TrainTrials: 1}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Errorf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate should error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// TestBatchTrainEvalConfig_WithDefaults verifies defaults are filled.
func TestBatchTrainEvalConfig_WithDefaults(t *testing.T) {
	cfg := BatchTrainEvalConfig{Domain: "d", Dataset: "d"}.WithDefaults()
	if cfg.Concurrency != 200 {
		t.Errorf("Concurrency = %d, want 200", cfg.Concurrency)
	}
	if cfg.MaxIterations != 30 {
		t.Errorf("MaxIterations = %d, want 30", cfg.MaxIterations)
	}
	if cfg.EvalSplit != "test" {
		t.Errorf("EvalSplit = %q, want 'test'", cfg.EvalSplit)
	}
	if cfg.Trials != 8 {
		t.Errorf("Trials = %d, want 8", cfg.Trials)
	}
	if cfg.RunTimestamp == "" {
		t.Errorf("RunTimestamp should be set")
	}
}

// TestListCaseLoader_Batches verifies the loader serves cases in
// batches of the configured size.
func TestListCaseLoader_Batches(t *testing.T) {
	cases := makeTestCases(5)
	loader := NewListCaseLoader("test", cases, 2)
	ctx := context.Background()
	var batches [][]Case
	for {
		batch, err := loader.NextBatch(ctx)
		if err != nil {
			if err == ErrEndOfBatches {
				break
			}
			t.Fatalf("NextBatch: %v", err)
		}
		batches = append(batches, batch)
	}
	if len(batches) != 3 {
		t.Fatalf("got %d batches, want 3", len(batches))
	}
	if len(batches[0]) != 2 || len(batches[1]) != 2 || len(batches[2]) != 1 {
		t.Errorf("batch sizes = %v", []int{len(batches[0]), len(batches[1]), len(batches[2])})
	}
	// Reset and re-iterate.
	if err := loader.Reset(ctx); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	batch, err := loader.NextBatch(ctx)
	if err != nil {
		t.Fatalf("NextBatch after Reset: %v", err)
	}
	if len(batch) != 2 {
		t.Errorf("after Reset, batch size = %d, want 2", len(batch))
	}
}

// TestListCaseLoader_DefaultBatchSize verifies batchSize=0 serves all
// cases in one batch.
func TestListCaseLoader_DefaultBatchSize(t *testing.T) {
	cases := makeTestCases(3)
	loader := NewListCaseLoader("test", cases, 0)
	batch, err := loader.NextBatch(context.Background())
	if err != nil {
		t.Fatalf("NextBatch: %v", err)
	}
	if len(batch) != 3 {
		t.Errorf("batch size = %d, want 3", len(batch))
	}
}

// TestContentHashSnapshotter_Deterministic verifies the same content
// produces the same snapshot ID.
func TestContentHashSnapshotter_Deterministic(t *testing.T) {
	s := NewContentHashSnapshotter()
	ps := PolicySet{
		RootURI:  "memory://root",
		Policies: []Policy{{Name: "p1", URI: "memory://p1", Version: 1, Status: PolicyStatusProduction, Content: "hello"}},
	}
	id1, err := s.Snapshot(context.Background(), ps, nil)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	id2, err := s.Snapshot(context.Background(), ps, nil)
	if err != nil {
		t.Fatalf("Snapshot 2: %v", err)
	}
	if id1 != id2 {
		t.Errorf("snapshot IDs differ: %s vs %s", id1, id2)
	}
	// Change content → different ID.
	ps.Policies[0].Content = "world"
	id3, err := s.Snapshot(context.Background(), ps, nil)
	if err != nil {
		t.Fatalf("Snapshot 3: %v", err)
	}
	if id1 == id3 {
		t.Errorf("snapshot IDs should differ after content change")
	}
}

// TestDryRunUpdater_RecordsPlan verifies the dry-run updater records
// the plan without applying it.
func TestDryRunUpdater_RecordsPlan(t *testing.T) {
	u := NewDryRunUpdater()
	plan := PolicyUpdatePlan{
		Items: []PolicyPlanItem{
			{Kind: PolicyPlanItemKindUpsert, TargetURI: "memory://a"},
			{Kind: PolicyPlanItemKindDelete, TargetURI: "memory://b"},
		},
	}
	ps := PolicySet{RootURI: "memory://root"}
	res, err := u.Apply(context.Background(), plan, ps, nil)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(res.WrittenURIs) != 1 || res.WrittenURIs[0] != "memory://a" {
		t.Errorf("WrittenURIs = %v", res.WrittenURIs)
	}
	if len(res.DeletedURIs) != 1 || res.DeletedURIs[0] != "memory://b" {
		t.Errorf("DeletedURIs = %v", res.DeletedURIs)
	}
	if u.ApplyCount != 1 {
		t.Errorf("ApplyCount = %d, want 1", u.ApplyCount)
	}
	if u.LastPlan.Items[0].TargetURI != "memory://a" {
		t.Errorf("LastPlan not recorded")
	}
}

// TestPatchSemanticGradient_TargetName verifies the name resolution
// from AfterFile.ExtraFields.
func TestPatchSemanticGradient_TargetName(t *testing.T) {
	cases := []struct {
		name string
		g    PatchSemanticGradient
		want string
	}{
		{
			name: "experience_name in extra_fields",
			g: PatchSemanticGradient{
				AfterFile: MemoryFile{
					URI:        "memory://exp/abc",
					MemoryType: "experiences",
					ExtraFields: map[string]any{
						"experience_name": "abc",
					},
				},
			},
			want: "abc",
		},
		{
			name: "fallback to uri slug",
			g: PatchSemanticGradient{
				AfterFile: MemoryFile{URI: "memory://exp/foo.md"},
			},
			want: "foo",
		},
		{
			name: "skill memory_type with skill_name",
			g: PatchSemanticGradient{
				AfterFile: MemoryFile{
					URI:        "memory://skill/bar",
					MemoryType: "skills",
					ExtraFields: map[string]any{
						"skill_name": "bar",
					},
				},
			},
			want: "bar",
		},
		{
			name: "no uri, no fields → unknown_policy",
			g:    PatchSemanticGradient{AfterFile: MemoryFile{}},
			want: "unknown_policy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.g.TargetName()
			if got != tc.want {
				t.Errorf("TargetName = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPatchSemanticGradient_TargetURI verifies AfterFile.URI wins
// over BeforeFile.URI.
func TestPatchSemanticGradient_TargetURI(t *testing.T) {
	g := PatchSemanticGradient{
		BeforeFile: &MemoryFile{URI: "memory://before"},
		AfterFile:  MemoryFile{URI: "memory://after"},
	}
	if got := g.TargetURI(); got != "memory://after" {
		t.Errorf("TargetURI = %q, want 'memory://after'", got)
	}
	// AfterFile.URI empty → fall back to BeforeFile.URI.
	g.AfterFile.URI = ""
	if got := g.TargetURI(); got != "memory://before" {
		t.Errorf("TargetURI = %q, want 'memory://before'", got)
	}
}

// TestPipeline_TrainEndToEnd verifies the full train pipeline: load
// cases → execute → analyze → estimate → plan → apply.
func TestPipeline_TrainEndToEnd(t *testing.T) {
	pipeline := makeTestPipeline()
	cases := makeTestCases(3)
	loader := NewListCaseLoader("test", cases, 2)
	ps := PolicySet{RootURI: "memory://root"}
	pc := PipelineContext{MaxEpochs: 1, LifecycleHooks: []PipelineLifecycleHook{&NoopLifecycleHook{}}}
	result, err := pipeline.Train(context.Background(), loader, ps, pc)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if len(result.Analyses) != 3 {
		t.Errorf("Analyses = %d, want 3", len(result.Analyses))
	}
	if len(result.Gradients) != 3 {
		t.Errorf("Gradients = %d, want 3", len(result.Gradients))
	}
	if len(result.Plan.Items) != 3 {
		t.Errorf("Plan Items = %d, want 3", len(result.Plan.Items))
	}
	if len(result.Epochs) != 1 {
		t.Errorf("Epochs = %d, want 1", len(result.Epochs))
	}
	if len(result.ApplyResult.WrittenURIs) != 3 {
		t.Errorf("WrittenURIs = %d, want 3", len(result.ApplyResult.WrittenURIs))
	}
}

// TestPipeline_TrainMultipleEpochs verifies the pipeline runs the
// configured number of epochs.
func TestPipeline_TrainMultipleEpochs(t *testing.T) {
	pipeline := makeTestPipeline()
	cases := makeTestCases(2)
	loader := NewListCaseLoader("test", cases, 0)
	ps := PolicySet{RootURI: "memory://root"}
	pc := PipelineContext{MaxEpochs: 3, LifecycleHooks: []PipelineLifecycleHook{&NoopLifecycleHook{}}}
	result, err := pipeline.Train(context.Background(), loader, ps, pc)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if len(result.Epochs) != 3 {
		t.Errorf("Epochs = %d, want 3", len(result.Epochs))
	}
	if len(result.Analyses) != 6 {
		t.Errorf("Analyses = %d, want 6 (2 cases × 3 epochs)", len(result.Analyses))
	}
}

// TestPipeline_TrainHookStopsEarly verifies a lifecycle hook can stop
// training after the Nth epoch.
func TestPipeline_TrainHookStopsEarly(t *testing.T) {
	pipeline := makeTestPipeline()
	cases := makeTestCases(1)
	loader := NewListCaseLoader("test", cases, 0)
	ps := PolicySet{RootURI: "memory://root"}
	hook := &countingHook{stopAt: 2}
	pc := PipelineContext{MaxEpochs: 5, LifecycleHooks: []PipelineLifecycleHook{hook}}
	result, err := pipeline.Train(context.Background(), loader, ps, pc)
	if err != nil {
		t.Fatalf("Train: %v", err)
	}
	if len(result.Epochs) != 2 {
		t.Errorf("Epochs = %d, want 2 (stopped at epoch 2)", len(result.Epochs))
	}
	if hook.epochEnds != 2 {
		t.Errorf("epochEnds = %d, want 2", hook.epochEnds)
	}
	if hook.trainEnds != 1 {
		t.Errorf("trainEnds = %d, want 1", hook.trainEnds)
	}
	if reason, _ := result.Metadata["stop_reason"].(string); reason != "test stop" {
		t.Errorf("stop_reason = %q, want 'test stop'", reason)
	}
}

// TestPipeline_EvalEndToEnd verifies eval-only mode: rollouts are
// executed and analyzed but no gradients are estimated and no plan
// is produced.
func TestPipeline_EvalEndToEnd(t *testing.T) {
	pipeline := makeTestPipeline()
	cases := makeTestCases(2)
	loader := NewListCaseLoader("test", cases, 0)
	ps := PolicySet{RootURI: "memory://root"}
	pc := PipelineContext{LifecycleHooks: []PipelineLifecycleHook{&NoopLifecycleHook{}}}
	res, err := pipeline.Eval(context.Background(), loader, ps, pc)
	if err != nil {
		t.Fatalf("Eval: %v", err)
	}
	if len(res.Analyses) != 2 {
		t.Errorf("Analyses = %d, want 2", len(res.Analyses))
	}
	if len(res.PolicySnapshotIDs) != 1 {
		t.Errorf("PolicySnapshotIDs = %v, want 1", res.PolicySnapshotIDs)
	}
}

// TestPipeline_TrainFromRollouts verifies the online training path.
func TestPipeline_TrainFromRollouts(t *testing.T) {
	pipeline := makeTestPipeline()
	cases := makeTestCases(2)
	rollouts := []Rollout{
		{Case: &cases[0], Messages: []Message{{Role: "assistant", Content: "x"}}},
		{Case: &cases[1], Messages: []Message{{Role: "assistant", Content: "y"}}},
	}
	ps := PolicySet{RootURI: "memory://root"}
	pc := PipelineContext{LifecycleHooks: []PipelineLifecycleHook{&NoopLifecycleHook{}}}
	res, err := pipeline.TrainFromRollouts(context.Background(), rollouts, ps, pc)
	if err != nil {
		t.Fatalf("TrainFromRollouts: %v", err)
	}
	if len(res.Analyses) != 2 {
		t.Errorf("Analyses = %d, want 2", len(res.Analyses))
	}
	if len(res.Plan.Items) != 2 {
		t.Errorf("Plan Items = %d, want 2", len(res.Plan.Items))
	}
}

// TestPipeline_TrainFromRollouts_WithCustomTrainer verifies the
// custom PolicyTrainer path is used when set.
func TestPipeline_TrainFromRollouts_WithCustomTrainer(t *testing.T) {
	pipeline := makeTestPipeline()
	pipeline.PolicyTrainer = &stubTrainer{}
	cases := makeTestCases(1)
	rollouts := []Rollout{{Case: &cases[0]}}
	ps := PolicySet{RootURI: "memory://root"}
	pc := PipelineContext{LifecycleHooks: []PipelineLifecycleHook{&NoopLifecycleHook{}}}
	res, err := pipeline.TrainFromRollouts(context.Background(), rollouts, ps, pc)
	if err != nil {
		t.Fatalf("TrainFromRollouts: %v", err)
	}
	if res.Metadata["via"] != "stub-trainer" {
		t.Errorf("Metadata = %v, want custom trainer path", res.Metadata)
	}
}

type stubTrainer struct{}

func (s *stubTrainer) TrainRollouts(ctx context.Context, rollouts []Rollout, ps PolicySet, _ any, _ []RolloutAnalysis) (RolloutTrainingResult, error) {
	return RolloutTrainingResult{Metadata: map[string]any{"via": "stub-trainer", "rollouts": len(rollouts)}}, nil
}

// TestPipeline_MissingComponents verifies validate catches nil
// components.
func TestPipeline_MissingComponents(t *testing.T) {
	p := &OfflinePolicyOptimizationPipeline{}
	_, err := p.Train(context.Background(), nil, PolicySet{}, PipelineContext{})
	if err == nil {
		t.Fatalf("Train should error with no components")
	}
	if !strings.Contains(err.Error(), "snapshotter") {
		t.Errorf("err = %v, want 'snapshotter'", err)
	}
}

// TestPipeline_ExecuteErrorPropagates verifies RolloutExecutor errors
// are returned.
func TestPipeline_ExecuteErrorPropagates(t *testing.T) {
	pipeline := makeTestPipeline()
	pipeline.RolloutExecutor.(*stubExecutor).failOn = 1
	pipeline.RolloutExecutor.(*stubExecutor).execErr = errors.New("boom")
	cases := makeTestCases(1)
	loader := NewListCaseLoader("test", cases, 0)
	ps := PolicySet{RootURI: "memory://root"}
	pc := PipelineContext{MaxEpochs: 1, LifecycleHooks: []PipelineLifecycleHook{&NoopLifecycleHook{}}}
	_, err := pipeline.Train(context.Background(), loader, ps, pc)
	if err == nil {
		t.Fatalf("Train should fail")
	}
	if !strings.Contains(err.Error(), "execute") {
		t.Errorf("err = %v, want 'execute'", err)
	}
}

// TestPipeline_AnalyzeErrorPropagates verifies RolloutAnalyzer errors
// are returned.
func TestPipeline_AnalyzeErrorPropagates(t *testing.T) {
	pipeline := makeTestPipeline()
	pipeline.RolloutAnalyzer.(*stubAnalyzer).failOn = 1
	pipeline.RolloutAnalyzer.(*stubAnalyzer).err = errors.New("analyze failed")
	cases := makeTestCases(1)
	loader := NewListCaseLoader("test", cases, 0)
	ps := PolicySet{RootURI: "memory://root"}
	pc := PipelineContext{MaxEpochs: 1, LifecycleHooks: []PipelineLifecycleHook{&NoopLifecycleHook{}}}
	_, err := pipeline.Train(context.Background(), loader, ps, pc)
	if err == nil {
		t.Fatalf("Train should fail")
	}
	if !strings.Contains(err.Error(), "engine") {
		t.Errorf("err = %v, want 'engine'", err)
	}
}

// TestPipeline_CtxCanceled verifies ctx cancellation propagates.
func TestPipeline_CtxCanceled(t *testing.T) {
	pipeline := makeTestPipeline()
	cases := makeTestCases(1)
	loader := NewListCaseLoader("test", cases, 0)
	ps := PolicySet{RootURI: "memory://root"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pc := PipelineContext{MaxEpochs: 1, LifecycleHooks: []PipelineLifecycleHook{&NoopLifecycleHook{}}}
	_, err := pipeline.Train(ctx, loader, ps, pc)
	if err == nil {
		t.Fatalf("Train should fail with canceled ctx")
	}
}

// TestLocalBatchRunner_EndToEnd verifies the local batch runner
// wires the pipeline together.
func TestLocalBatchRunner_EndToEnd(t *testing.T) {
	pipeline := makeTestPipeline()
	cases := makeTestCases(2)
	loader := NewListCaseLoader("test", cases, 0)
	ps := PolicySet{RootURI: "memory://root"}
	runner := &LocalBatchRunner{
		Pipeline:   pipeline,
		CaseLoader: loader,
		PolicySet:  ps,
	}
	cfg := BatchTrainEvalConfig{
		Domain:  "test-domain",
		Dataset: "test-dataset",
		Epochs:  2,
		Trials:  1,
	}.WithDefaults()
	report, err := RunBatchTrainEval(context.Background(), cfg, runner)
	if err != nil {
		t.Fatalf("RunBatchTrainEval: %v", err)
	}
	if report.Domain != "test-domain" {
		t.Errorf("Domain = %q", report.Domain)
	}
	if report.Epochs != 2 {
		t.Errorf("Epochs = %d, want 2", report.Epochs)
	}
	if epochs, _ := report.Metadata["epochs_run"].(int); epochs != 2 {
		t.Errorf("epochs_run = %v, want 2", report.Metadata["epochs_run"])
	}
	if report.Duration() <= 0 {
		t.Errorf("Duration should be positive")
	}
}

// TestRunBatchTrainEval_NilRunner verifies a nil runner errors.
func TestRunBatchTrainEval_NilRunner(t *testing.T) {
	_, err := RunBatchTrainEval(context.Background(), BatchTrainEvalConfig{Domain: "d", Dataset: "d"}, nil)
	if err == nil {
		t.Fatalf("should error with nil runner")
	}
	if !strings.Contains(err.Error(), "nil") {
		t.Errorf("err = %v", err)
	}
}

// TestRunBatchTrainEval_InvalidConfig verifies bad config errors.
func TestRunBatchTrainEval_InvalidConfig(t *testing.T) {
	runner := &LocalBatchRunner{}
	_, err := RunBatchTrainEval(context.Background(), BatchTrainEvalConfig{}, runner)
	if err == nil {
		t.Fatalf("should error with empty config")
	}
	if !strings.Contains(err.Error(), "dataset") {
		t.Errorf("err = %v", err)
	}
}

// TestSemaphore_AcquireRelease verifies the semaphore bounds
// concurrency.
func TestSemaphore_AcquireRelease(t *testing.T) {
	s := makeSemaphore(2, 10)
	ctx := context.Background()
	s.acquire(ctx)
	s.acquire(ctx)
	// Third acquire should block; release one then try again.
	go func() {
		time.Sleep(10 * time.Millisecond)
		s.release()
	}()
	s.acquire(ctx) // should succeed after the release
	s.release()
	s.release()
}

// TestNoopLifecycleHook verifies the no-op hook lets runs continue.
func TestNoopLifecycleHook(t *testing.T) {
	h := &NoopLifecycleHook{}
	dec, err := h.OnEpochEnd(context.Background(), 1, PipelineEpochResult{})
	if err != nil {
		t.Fatalf("OnEpochEnd: %v", err)
	}
	if dec.StopTraining {
		t.Errorf("StopTraining = true, want false")
	}
	if h.Name() != "noop" {
		t.Errorf("Name = %q, want 'noop'", h.Name())
	}
}
