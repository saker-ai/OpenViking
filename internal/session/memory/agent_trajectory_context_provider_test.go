// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrajectoryMemoryTypeConstant(t *testing.T) {
	t.Parallel()
	// TrajectoryMemoryType is defined in constants.go and reused.
	assert.Equal(t, "trajectories", TrajectoryMemoryType)
	assert.True(t, IsExecutionMemoryType(TrajectoryMemoryType))
}

func TestAgentTrajectoryContextProvider_Instruction(t *testing.T) {
	t.Parallel()
	p := NewAgentTrajectoryContextProvider(nil, true, false)
	instr := p.Instruction()
	assert.Contains(t, instr, "extraction agent")
}

func TestAgentTrajectoryContextProvider_GetMemorySchemas_TrajectoryOnly(t *testing.T) {
	t.Parallel()
	p := NewAgentTrajectoryContextProvider(nil, true, false)
	schemas := p.GetMemorySchemas(nil)
	// The default registry may or may not have the trajectory schema enabled.
	// Just verify no panic and the right type returned.
	for _, s := range schemas {
		assert.Equal(t, TrajectoryMemoryType, s.MemoryType)
	}
}

func TestAgentTrajectoryContextProvider_GetMemorySchemas_WithSkills(t *testing.T) {
	t.Parallel()
	p := NewAgentTrajectoryContextProvider(nil, true, true)
	schemas := p.GetMemorySchemas(nil)
	types := make(map[string]bool, len(schemas))
	for _, s := range schemas {
		types[s.MemoryType] = true
	}
	// When skills are included, we expect at most both types.
	if len(schemas) > 1 {
		assert.True(t, types[TrajectoryMemoryType])
	}
}

func TestAgentTrajectoryContextProvider_GetTools_WithSkills(t *testing.T) {
	t.Parallel()
	p := NewAgentTrajectoryContextProvider(nil, false, true)
	tools := p.GetTools()
	assert.Equal(t, []string{"read"}, tools)
}

func TestAgentTrajectoryContextProvider_GetTools_WithoutSkills(t *testing.T) {
	t.Parallel()
	p := NewAgentTrajectoryContextProvider(nil, false, false)
	tools := p.GetTools()
	assert.Nil(t, tools)
}

func TestAgentTrajectoryContextProvider_Prefetch_NoMessages_NoSkills(t *testing.T) {
	t.Parallel()
	p := NewAgentTrajectoryContextProvider(nil, true, false)
	out, err := p.Prefetch(context.Background())
	require.NoError(t, err)
	assert.Nil(t, out)
}

func TestAgentTrajectoryContextProvider_Prefetch_WithMessages(t *testing.T) {
	t.Parallel()
	msgs := []Message{
		NewTextMessage("1", "user", "", "2026-01-01T00:00:00Z",
			[]MessagePart{NewTextOnlyPart("hello")}),
	}
	p := NewAgentTrajectoryContextProvider(msgs, true, false)
	out, err := p.Prefetch(context.Background())
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, "user", out[0]["role"])
}

func TestAgentTrajectoryContextProvider_ExecuteTool_NoSkills(t *testing.T) {
	t.Parallel()
	p := NewAgentTrajectoryContextProvider(nil, true, false)
	result, err := p.ExecuteTool(context.Background(), ToolCall{Name: "read"})
	assert.NoError(t, err)
	m, ok := result.(map[string]any)
	require.True(t, ok)
	assert.Contains(t, m["error"].(string), "Unknown tool")
}

func TestSharedSkillStateKeys(t *testing.T) {
	t.Parallel()
	keys := SharedSkillStateKeys()
	assert.NotEmpty(t, keys)
	// Must be sorted.
	for i := 1; i < len(keys); i++ {
		assert.True(t, keys[i-1] <= keys[i], "keys not sorted: %v", keys)
	}
	assert.Contains(t, keys, "messages")
	assert.Contains(t, keys, "_ctx")
}

func TestSessionSkillMemoryType(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "session_skills", SessionSkillMemoryType)
}
