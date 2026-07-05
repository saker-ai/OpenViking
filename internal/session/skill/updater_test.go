// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package skill

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sessionmemory "github.com/saker-ai/ctxhub/internal/session/memory"
)

func TestNewSkillOperationUpdater(t *testing.T) {
	t.Parallel()
	u := NewSkillOperationUpdater(nil, nil, nil)
	assert.NotNil(t, u)
}

func TestNewSkillOperationUpdater_WithRegistry(t *testing.T) {
	t.Parallel()
	reg := sessionmemory.NewMemoryTypeRegistry()
	u := NewSkillOperationUpdater(reg, nil, nil)
	assert.NotNil(t, u)
	assert.Equal(t, reg, u.registry)
}

func TestSkillOperationUpdater_ApplyOperations_NotReady(t *testing.T) {
	t.Parallel()
	u := NewSkillOperationUpdater(nil, nil, nil)
	_, err := u.ApplyOperations(ResolvedOperations{}, nil)
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrUpdaterNotReady), "expected ErrUpdaterNotReady, got %v", err)
}

func TestErrUpdaterNotReady_Message(t *testing.T) {
	t.Parallel()
	assert.Contains(t, ErrUpdaterNotReady.Error(), "not ready")
	assert.Contains(t, ErrUpdaterNotReady.Error(), "P0-2")
}

func TestSkillOperationUpdateResult_AddWritten(t *testing.T) {
	t.Parallel()
	r := &SkillOperationUpdateResult{}
	r.AddWritten("a.md")
	r.AddWritten("b.md")
	assert.Equal(t, []string{"a.md", "b.md"}, r.WrittenURIs)
}

func TestSkillOperationUpdateResult_AddEdited(t *testing.T) {
	t.Parallel()
	r := &SkillOperationUpdateResult{}
	r.AddEdited("a.md")
	assert.Equal(t, []string{"a.md"}, r.EditedURIs)
}

func TestSkillOperationUpdateResult_AddError(t *testing.T) {
	t.Parallel()
	r := &SkillOperationUpdateResult{}
	customErr := errors.New("boom")
	r.AddError("a.md", customErr)
	require.Len(t, r.Errors, 1)
	assert.Equal(t, "a.md", r.Errors[0].URI)
	assert.Equal(t, customErr, r.Errors[0].Error)
}

func TestSkillOperationUpdateResult_AddResult(t *testing.T) {
	t.Parallel()
	r := &SkillOperationUpdateResult{}
	r.AddResult(map[string]any{"action": "create"})
	require.Len(t, r.OperationResults, 1)
	assert.Equal(t, "create", r.OperationResults[0]["action"])
}
