// Package train — concrete minimal implementations of the protocol
// interfaces for testing and bootstrap. These mirror the Python
// components/case_loader.py (ListCaseLoader), components/snapshotter.py
// (ContentHashPolicySnapshotter), and components/policy_updater.py
// (DryRunPolicyUpdater).
//
// Heavy LLM-backed components (SingleTurnLLMRolloutExecutor,
// ExperienceGradientEstimator, TrajectoryRolloutAnalyzer,
// PatchMergePolicyOptimizer) are intentionally NOT implemented here —
// they require LLM clients, prompt engineering, and storage adapters
// that operators supply. The Go framework provides the orchestration
// glue; operators plug in concrete strategies.

package train

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// ListCaseLoader serves a fixed list of cases in batches. It is the
// simplest CaseLoader — useful for tests and for small offline runs
// where the dataset fits in memory.
type ListCaseLoader struct {
	name   string
	cases  []Case
	batch  int
	mu     sync.Mutex
	pos    int
}

// NewListCaseLoader constructs a loader that serves cases in batches
// of batchSize. When batchSize <= 0, all cases are served in one batch.
func NewListCaseLoader(name string, cases []Case, batchSize int) *ListCaseLoader {
	if batchSize <= 0 {
		batchSize = len(cases)
		if batchSize == 0 {
			batchSize = 1
		}
	}
	return &ListCaseLoader{name: name, cases: cases, batch: batchSize}
}

// NextBatch returns the next slice of cases. Returns ErrEndOfBatches
// when exhausted.
func (l *ListCaseLoader) NextBatch(ctx context.Context) ([]Case, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.pos >= len(l.cases) {
		return nil, ErrEndOfBatches
	}
	end := l.pos + l.batch
	if end > len(l.cases) {
		end = len(l.cases)
	}
	batch := l.cases[l.pos:end]
	l.pos = end
	return batch, nil
}

// Reset repositions the loader at the start of the dataset.
func (l *ListCaseLoader) Reset(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pos = 0
	return nil
}

// Name returns the loader's identifier.
func (l *ListCaseLoader) Name() string { return l.name }

// ContentHashPolicySnapshotter produces a content-hash snapshot ID
// from the policy set's contents. Two PolicySets with the same content
// produce the same snapshot ID, which lets eval runs reuse cached
// rollouts.
type ContentHashPolicySnapshotter struct{}

// NewContentHashSnapshotter constructs a ContentHashPolicySnapshotter.
func NewContentHashSnapshotter() *ContentHashPolicySnapshotter {
	return &ContentHashPolicySnapshotter{}
}

// Snapshot returns a sha256 hash of the policy set's content. The hash
// includes RootURI and each policy's {Name, URI, Version, Status,
// Content} tuple, so any update to content or version produces a new
// snapshot ID.
func (s *ContentHashPolicySnapshotter) Snapshot(ctx context.Context, policySet PolicySet, _ any) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "root=%s\n", policySet.RootURI)
	for _, p := range policySet.Policies {
		fmt.Fprintf(h, "policy=%s|uri=%s|v=%d|status=%s|content=%s\n",
			p.Name, p.URI, p.Version, p.Status, p.Content)
	}
	return "snap-" + hex.EncodeToString(h.Sum(nil))[:16], nil
}

// DryRunPolicyUpdater records apply calls without writing anything.
// Useful for tests and for benchmark runs that want to inspect the
// plan without committing changes.
type DryRunPolicyUpdater struct {
	mu        sync.Mutex
	LastPlan  PolicyUpdatePlan
	LastSet   PolicySet
	ApplyCount int
}

// NewDryRunUpdater constructs a DryRunPolicyUpdater.
func NewDryRunUpdater() *DryRunPolicyUpdater {
	return &DryRunPolicyUpdater{}
}

// Apply records the plan and returns a no-op result. The returned
// PolicyApplyResult has UpdatedPolicySet equal to the input (no
// changes applied) and WrittenURIs/DeletedURIs populated from the
// plan items so callers can inspect what would have been written.
func (u *DryRunPolicyUpdater) Apply(ctx context.Context, plan PolicyUpdatePlan, policySet PolicySet, _ any) (PolicyApplyResult, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.LastPlan = plan
	u.LastSet = policySet
	u.ApplyCount++
	var written, deleted []string
	for _, item := range plan.Items {
		switch item.Kind {
		case PolicyPlanItemKindUpsert:
			if item.TargetURI != "" {
				written = append(written, item.TargetURI)
			}
		case PolicyPlanItemKindDelete:
			if item.TargetURI != "" {
				deleted = append(deleted, item.TargetURI)
			}
		}
	}
	return PolicyApplyResult{
		UpdatedPolicySet: policySet,
		WrittenURIs:      written,
		DeletedURIs:      deleted,
		Errors:           nil,
		Metadata:         map[string]any{"dry_run": true},
	}, nil
}

// Compile-time assertions.
var (
	_ CaseLoader         = (*ListCaseLoader)(nil)
	_ PolicySnapshotter  = (*ContentHashPolicySnapshotter)(nil)
	_ PolicyUpdater      = (*DryRunPolicyUpdater)(nil)
)
