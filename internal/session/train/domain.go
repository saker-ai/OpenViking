// Package train implements the offline policy optimization pipeline
// for the session memory/skill framework. It mirrors the Python
// `openviking/session/train/` package: domain types, protocol
// interfaces, a PolicyTrainingEngine that runs the
// analyze→estimate→plan→apply loop, an OfflinePolicyOptimizationPipeline
// that wires interfaces into train/eval flows, and a BatchTrainEval
// orchestrator that drives remote benchmark runs.
//
// The framework is intentionally composable: heavy LLM-backed
// components (rollout execution, trajectory analysis, gradient
// estimation, policy optimization) are exposed as interfaces.
// Operators plug in concrete strategies; the framework provides the
// orchestration glue and a small set of deterministic, network-free
// implementations (ListCaseLoader, ContentHashPolicySnapshotter,
// DryRunPolicyUpdater) for testing and bootstrap.
package train

import (
	"fmt"
	"time"
)

// PolicyStatus is the lifecycle state of a policy file in a PolicySet.
type PolicyStatus string

const (
	PolicyStatusDraft      PolicyStatus = "draft"
	PolicyStatusStaging    PolicyStatus = "staging"
	PolicyStatusProduction PolicyStatus = "production"
	PolicyStatusDeprecated PolicyStatus = "deprecated"
	PolicyStatusArchived   PolicyStatus = "archived"
)

// TrajectoryOutcome is the success/failure label of a trajectory.
type TrajectoryOutcome string

const (
	TrajectoryOutcomeSuccess    TrajectoryOutcome = "success"
	TrajectoryOutcomeFailure    TrajectoryOutcome = "failure"
	TrajectoryOutcomePartial    TrajectoryOutcome = "partial"
	TrajectoryOutcomeUnfinished TrajectoryOutcome = "unfinished"
	TrajectoryOutcomeUnknown    TrajectoryOutcome = "unknown"
)

// PolicyPlanItemKind is the upsert/delete tag for plan items.
type PolicyPlanItemKind string

const (
	PolicyPlanItemKindUpsert PolicyPlanItemKind = "upsert"
	PolicyPlanItemKindDelete PolicyPlanItemKind = "delete"
)

// Policy is a single trainable memory file in a PolicySet. Generic
// over memory_type (experiences, skills, etc.); type-specific fields
// live in Metadata.
type Policy struct {
	Name      string
	URI       string
	Version   int
	Status    PolicyStatus
	Content   string
	Metadata  map[string]any
	Links     []map[string]any
	Backlinks []map[string]any
}

// PolicySet is a snapshot of all policies under a policy root URI.
// It is the unit of train/eval: each epoch reloads the set from the
// backing store, applies updates, and re-snapshots.
type PolicySet struct {
	RootURI   string
	Policies  []Policy
	Metadata  map[string]any
	// VikingFS and RequestContext are runtime storage dependencies
	// used for concurrency-safe policy updates. They are intentionally
	// typed as any so the train package does not pull in the storage
	// package; concrete loaders up-cast as needed.
	VikingFS       any
	RequestContext any
}

// Trajectory is a distilled, trainable trajectory sample.
type Trajectory struct {
	Name            string
	URI             string
	Content         string
	Outcome         TrajectoryOutcome
	RetrievalAnchor string
	Metadata        map[string]any
}

// RubricCriterion is one scored criterion in a case rubric.
type RubricCriterion struct {
	Name        string
	Description string
	Required    bool
	Weight      float64
	Metadata    map[string]any
}

// Rubric is the acceptance criteria for a case.
type Rubric struct {
	Name        string
	Description string
	Criteria    []RubricCriterion
	Metadata    map[string]any
}

// Case is an executable, reproducible, evaluable training/evaluation
// sample.
type Case struct {
	Name           string
	TaskSignature  string
	Input          map[string]any
	Rubric         Rubric
	Metadata       map[string]any
}

// Message is a single turn in a rollout. Mirrors the Python
// openviking.message.Message shape (role + content + metadata).
type Message struct {
	Role     string
	Content  string
	Metadata map[string]any
}

// Rollout is the execution record for a case under a policy snapshot.
type Rollout struct {
	Case              *Case
	Messages          []Message
	PolicySnapshotID  string
	Evaluation        *RubricEvaluation
	Metadata          map[string]any
}

// CriterionResult is the evaluation result for one rubric criterion.
type CriterionResult struct {
	CriterionName string
	Passed        bool
	Score         float64
	Feedback      []string
	Evidence      []string
	Metadata      map[string]any
}

// RubricEvaluation is the structured evaluation of a rollout against
// a rubric.
type RubricEvaluation struct {
	Passed           bool
	Score            float64
	CriterionResults []CriterionResult
	Feedback         []string
	Metadata         map[string]any
}

// RolloutAnalysis bundles the rubric evaluation with trajectories
// extracted from the same rollout, plus any pre-extracted gradients
// (e.g. session skill patches) that bypass the gradient estimator.
type RolloutAnalysis struct {
	Evaluation  RubricEvaluation
	Trajectories []Trajectory
	Gradients   []PatchSemanticGradient
	Metadata    map[string]any
}

// PolicyPlanItem is one executable upsert/delete against a target
// policy file.
type PolicyPlanItem struct {
	Kind            PolicyPlanItemKind
	MemoryType      string
	TargetName      string
	TargetURI       string
	BeforeContent   *string
	AfterContent    *string
	BaseVersion     *int
	Confidence      *float64
	Links           []StoredLink
	Metadata        map[string]any
}

// PolicyUpdatePlan is the planned update for a PolicySet.
type PolicyUpdatePlan struct {
	Items    []PolicyPlanItem
	Metadata map[string]any
}

// PolicyApplyResult is the result of applying a PolicyUpdatePlan.
type PolicyApplyResult struct {
	UpdatedPolicySet PolicySet
	WrittenURIs      []string
	DeletedURIs      []string
	Errors           []string
	Metadata         map[string]any
}

// PipelineEpochResult is one rollout→evaluate→train epoch.
type PipelineEpochResult struct {
	Epoch              int
	Analyses           []RolloutAnalysis
	Gradients          []PatchSemanticGradient
	Plan               PolicyUpdatePlan
	ApplyResult        PolicyApplyResult
	PolicySnapshotIDs  []string
	Metadata           map[string]any
}

// PipelineEvaluationResult is an eval-only pass over a snapshot.
type PipelineEvaluationResult struct {
	Epoch              int
	Analyses           []RolloutAnalysis
	PolicySnapshotIDs  []string
	Metadata           map[string]any
}

// PipelineResult is the end-to-end result of a train call.
type PipelineResult struct {
	Analyses        []RolloutAnalysis
	Gradients       []PatchSemanticGradient
	Plan            PolicyUpdatePlan
	ApplyResult     PolicyApplyResult
	Epochs          []PipelineEpochResult
	EvaluationPasses []PipelineEvaluationResult
	Metadata        map[string]any
}

// RolloutTrainingResult is the result of training from externally
// produced rollouts (online/realtime counterpart of one offline epoch).
type RolloutTrainingResult struct {
	Analyses    []RolloutAnalysis
	Gradients   []PatchSemanticGradient
	Plan        PolicyUpdatePlan
	ApplyResult PolicyApplyResult
	Metadata    map[string]any
}

// StoredLink is a typed pointer between memory files. Mirrors the
// Python openviking.session.memory.dataclass.StoredLink shape.
type StoredLink struct {
	Rel      string
	Target   string
	Metadata map[string]any
}

// BatchTrainEvalConfig configures one remote benchmark batch train/eval
// run. Mirrors the Python BatchTrainEvalConfig dataclass field-for-field
// (with Go-idiomatic naming). The orchestrator interprets these fields
// when calling a remote ctxhub-server benchmark endpoint.
type BatchTrainEvalConfig struct {
	Domain                     string
	Dataset                    string
	Epochs                     int
	BatchSize                  *int
	Concurrency                int
	ConfigPath                 string
	OutputPath                 string
	KeepDefaultTools           bool
	MaxIterations              int
	ServerURL                  string
	APIKey                     string
	AccountID                  string
	UserID                     string
	CommitKeepRecentCount      int
	CommitPollIntervalSeconds  float64
	CommitTimeoutSeconds       *float64
	CommitConcurrency          int
	TrainIndex                 any // int | string | []int
	EvalIndex                  any // int | string | []int
	BenchmarkServiceURL        string
	BaselineForceRecompute     bool
	SkipBaselineEval           bool
	EvalEachEpoch              bool
	EvalSplit                  string
	SkipFinalEval              bool
	Trials                     int
	TrainTrials                int
	ReuseTrainRolloutCache     bool
	CleanResult                bool
	KeepRecentResults          int
	EventsPath                 string
	ResultDirName              string
	RunTimestamp               string
}

// Validate returns an error if the config has inconsistent fields.
// Mirrors the Python __post_init__ validation.
func (c BatchTrainEvalConfig) Validate() error {
	if c.Dataset == "" {
		return fmt.Errorf("train: dataset is required")
	}
	if c.Domain == "" {
		return fmt.Errorf("train: domain is required")
	}
	if c.Epochs < 0 {
		return fmt.Errorf("train: epochs must be >= 0, got %d", c.Epochs)
	}
	if c.BatchSize != nil && *c.BatchSize <= 0 {
		return fmt.Errorf("train: batch_size must be > 0, got %d", *c.BatchSize)
	}
	if c.Concurrency <= 0 {
		return fmt.Errorf("train: concurrency must be > 0, got %d", c.Concurrency)
	}
	if c.Trials <= 0 {
		return fmt.Errorf("train: trials must be > 0, got %d", c.Trials)
	}
	if c.TrainTrials <= 0 {
		return fmt.Errorf("train: train_trials must be > 0, got %d", c.TrainTrials)
	}
	if c.RunTimestamp == "" {
		c.RunTimestamp = time.Now().Format("20060102_150405")
	}
	return nil
}

// WithDefaults returns a copy of c with default values filled in for
// unset fields. Mirrors the Python dataclass default_factory behavior.
func (c BatchTrainEvalConfig) WithDefaults() BatchTrainEvalConfig {
	if c.Concurrency == 0 {
		c.Concurrency = 200
	}
	if c.MaxIterations == 0 {
		c.MaxIterations = 30
	}
	if c.AccountID == "" {
		c.AccountID = "default"
	}
	if c.UserID == "" {
		c.UserID = "default"
	}
	if c.CommitPollIntervalSeconds == 0 {
		c.CommitPollIntervalSeconds = 2.0
	}
	if c.CommitConcurrency == 0 {
		c.CommitConcurrency = 200
	}
	if c.EvalSplit == "" {
		c.EvalSplit = "test"
	}
	if c.Trials == 0 {
		c.Trials = 8
	}
	if c.TrainTrials == 0 {
		c.TrainTrials = 1
	}
	if c.ResultDirName == "" {
		c.ResultDirName = "train"
	}
	if c.RunTimestamp == "" {
		c.RunTimestamp = time.Now().Format("20060102_150405")
	}
	return c
}

// BatchTrainEvalReport is the result of a batch train/eval run. It
// summarizes what was run and where the artifacts live.
type BatchTrainEvalReport struct {
	Domain       string
	Dataset      string
	Epochs       int
	Trials       int
	OutputPath   string
	EventsPath   string
	RunTimestamp string
	StartedAt    time.Time
	FinishedAt   time.Time
	Metadata     map[string]any
}

// Duration returns the wall-clock duration of the run.
func (r BatchTrainEvalReport) Duration() time.Duration {
	return r.FinishedAt.Sub(r.StartedAt)
}
