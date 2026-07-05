// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package skill

import (
	sessionmemory "github.com/saker-ai/ctxhub/internal/session/memory"
)

// SkillOperationUpdater applies resolved ReAct skill operations to real
// skill assets. It is the Go counterpart of
// openviking.session.skill.skill_operation_updater.SkillOperationUpdater.
//
// STATUS: stub. The full implementation depends on the session/memory/
// pipeline (MergeOpFactory, ContentWriteCoordinator) and the
// SkillProcessor, both of which land with the rest of P0-2. Until
// then, the surface here is intentionally minimal so callers can
// compile against the type and the dedup pass in dedup.go can run
// end-to-end.
//
// The registry field is now typed to *sessionmemory.MemoryTypeRegistry
// (the canonical registry type that landed with P0-2 Tier 1). The
// skillProcessor and fs fields remain `any` until their concrete types
// land in the Go tree.
type SkillOperationUpdater struct {
	// registry is the memory type registry used to look up merge ops
	// and templates for skill operations.
	registry *sessionmemory.MemoryTypeRegistry
	// skillProcessor will be *skillprocessor.Processor once that
	// package lands.
	skillProcessor any
	// fs will be ragfs.FileSystem (already present in the Go tree).
	fs any
}

// NewSkillOperationUpdater returns an updater wired to the given
// collaborators. registry may be nil for callers that only need the
// dedup pass; skillProcessor and fs remain `any` until their concrete
// types land.
func NewSkillOperationUpdater(registry *sessionmemory.MemoryTypeRegistry, skillProcessor, fs any) *SkillOperationUpdater {
	return &SkillOperationUpdater{registry: registry, skillProcessor: skillProcessor, fs: fs}
}

// SkillOperationUpdateResult records the outcome of applying one batch
// of operations. Mirrors SkillOperationUpdateResult in the Python file.
type SkillOperationUpdateResult struct {
	WrittenURIs      []string
	EditedURIs       []string
	Errors           []SkillOpError
	OperationResults []map[string]any
}

// SkillOpError pairs a target URI with the error that occurred.
type SkillOpError struct {
	URI   string
	Error error
}

// AddWritten appends a created-skill URI.
func (r *SkillOperationUpdateResult) AddWritten(uri string) { r.WrittenURIs = append(r.WrittenURIs, uri) }

// AddEdited appends an updated-skill URI.
func (r *SkillOperationUpdateResult) AddEdited(uri string) { r.EditedURIs = append(r.EditedURIs, uri) }

// AddError records a per-URI failure.
func (r *SkillOperationUpdateResult) AddError(uri string, err error) {
	r.Errors = append(r.Errors, SkillOpError{URI: uri, Error: err})
}

// AddResult appends one per-operation summary map.
func (r *SkillOperationUpdateResult) AddResult(result map[string]any) {
	r.OperationResults = append(r.OperationResults, result)
}

// ApplyOperations applies a batch of resolved operations to the
// configured skill assets. NOT IMPLEMENTED in this stub — it returns
// ErrUpdaterNotReady so callers can detect the missing wiring without
// crashing.
//
// The full implementation will:
//  1. Surface ResolvedOperations.Errors as AddError entries.
//  2. For each upsert, load any existing SKILL.md at op.URIs[0].
//  3. Merge the operation's fields over the existing skill using the
//     schema's merge ops (MergeOpFactory.from_field).
//  4. On create, run SkillProcessor.process_skill; on update, run
//     SkillProcessor.sanitize_skill_privacy and ContentWriteCoordinator.
//  5. Append {status, action, root_uri, uri, skill_md_uri, name} maps
//     to OperationResults.
func (u *SkillOperationUpdater) ApplyOperations(_ ResolvedOperations, _ any) (*SkillOperationUpdateResult, error) {
	return nil, ErrUpdaterNotReady
}

// ErrUpdaterNotReady is returned by ApplyOperations until P0-2 lands.
var ErrUpdaterNotReady = errUpdaterNotReady{}

type errUpdaterNotReady struct{}

func (errUpdaterNotReady) Error() string {
	return "skill: SkillOperationUpdater not ready (depends on P0-2 session/memory/)"
}
