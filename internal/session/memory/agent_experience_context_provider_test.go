// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExperienceMemoryTypeConstant(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "experiences", ExperienceMemoryType)
	assert.True(t, IsExecutionMemoryType(ExperienceMemoryType))
}

func TestExperienceSearchTopKConstant(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 5, ExperienceSearchTopK)
	assert.Equal(t, 3, ExperienceSourceTrajTopK)
	assert.Equal(t, 3, ExperienceMaxSourceTrajs)
}

func TestAgentExperienceContextProvider_Instruction(t *testing.T) {
	t.Parallel()
	p := NewAgentExperienceContextProvider(nil, "summary", "viking://user/u/memories/trajectories/t.md", "")
	instr := p.Instruction()
	assert.Contains(t, instr, "memory extraction agent")
	assert.Contains(t, instr, "experience_name")
	assert.Contains(t, instr, "supersedes")
}

func TestAgentExperienceContextProvider_GetTools(t *testing.T) {
	t.Parallel()
	p := NewAgentExperienceContextProvider(nil, "", "", "")
	assert.Nil(t, p.GetTools())
}

func TestAgentExperienceContextProvider_GetMemorySchemas(t *testing.T) {
	t.Parallel()
	p := NewAgentExperienceContextProvider(nil, "", "", "")
	schemas := p.GetMemorySchemas(nil)
	for _, s := range schemas {
		assert.Equal(t, ExperienceMemoryType, s.MemoryType)
	}
}

func TestAgentExperienceContextProvider_Prefetch_NoMessages(t *testing.T) {
	t.Parallel()
	p := NewAgentExperienceContextProvider(nil, "summary", "uri", "")
	out, err := p.Prefetch(context.Background())
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestAgentExperienceContextProvider_Prefetch_WithMessages(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("I had a problem with my flight")}),
	}
	p := NewAgentExperienceContextProvider(msgs, "summary", "viking://user/u/memories/trajectories/t.md", "")
	out, err := p.Prefetch(context.Background())
	require.NoError(t, err)
	// Even without a viking FS, we expect at least the conversation message +
	// the new-trajectory tool-call pair.
	assert.NotEmpty(t, out)
	assert.Equal(t, "user", out[0]["role"])
}

func TestAgentExperienceContextProvider_RenderExperienceDir(t *testing.T) {
	t.Parallel()
	p := NewAgentExperienceContextProvider(nil, "", "", "")
	// Without context, the rendered dir uses the default user space.
	dir := p.renderExperienceDir()
	// The registry may or may not have the experiences schema configured.
	// Just verify no panic.
	_ = dir
}

func TestAgentExperienceContextProvider_FallbackListExperiences_NilFS(t *testing.T) {
	t.Parallel()
	p := NewAgentExperienceContextProvider(nil, "", "", "")
	// With nil FS, Ls returns an error, so result is nil.
	result := p.fallbackListExperiences(context.Background(), "viking://user/u/memories/experiences")
	assert.Nil(t, result)
}

func TestAgentExperienceContextProvider_LoadSourceTrajectories_NilFS(t *testing.T) {
	t.Parallel()
	p := NewAgentExperienceContextProvider(nil, "", "", "")
	links := []map[string]any{
		{"link_type": "derived_from", "to_uri": "viking://user/u/memories/trajectories/x.md"},
	}
	result := p.loadSourceTrajectories(context.Background(), "viking://user/u/memories/experiences/e.md", links)
	// With nil FS, ReadFile returns error, so empty result.
	assert.Empty(t, result)
}
