// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"fmt"
	"strings"
)

// AgentTrajectoryContextProvider is Phase 1 of agent-scope extraction.
// It extracts execution trajectories from the conversation. It mirrors
// openviking.session.memory.agent_trajectory_context_provider.AgentTrajectoryContextProvider.
//
// TrajectoryMemoryType is defined in constants.go and is reused here.
type AgentTrajectoryContextProvider struct {
	*SessionExtractContextProvider

	IncludeTrajectories  bool
	IncludeSessionSkills bool
}

// NewAgentTrajectoryContextProvider constructs the provider. When
// includeTrajectories is true, the trajectory schema is loaded; when
// includeSessionSkills is true, the session-skill schema is also loaded.
// It mirrors AgentTrajectoryContextProvider.__init__.
func NewAgentTrajectoryContextProvider(
	messages []Message,
	includeTrajectories, includeSessionSkills bool,
) *AgentTrajectoryContextProvider {
	return &AgentTrajectoryContextProvider{
		SessionExtractContextProvider: NewSessionExtractContextProvider(messages, ""),
		IncludeTrajectories:           includeTrajectories,
		IncludeSessionSkills:          includeSessionSkills,
	}
}

// Instruction returns the system prompt. It mirrors
// AgentTrajectoryContextProvider.instruction.
func (p *AgentTrajectoryContextProvider) Instruction() string {
	return "You are an extraction agent. Analyze the archived conversation, use read when " +
		"needed, and output only JSON that matches the schema descriptions."
}

// GetMemorySchemas returns the trajectory schema and optionally the
// session-skill schema. It mirrors
// AgentTrajectoryContextProvider.get_memory_schemas.
func (p *AgentTrajectoryContextProvider) GetMemorySchemas(requestCtx any) []MemoryTypeSchema {
	registry := p.getRegistry()
	memoryTypes := make([]string, 0, 2)
	if p.IncludeTrajectories {
		memoryTypes = append(memoryTypes, TrajectoryMemoryType)
	}
	if p.IncludeSessionSkills {
		memoryTypes = append(memoryTypes, SessionSkillMemoryType)
	}
	schemas := make([]MemoryTypeSchema, 0, len(memoryTypes))
	for _, mt := range memoryTypes {
		schema, ok := registry.Get(mt)
		if !ok || !schema.Enabled {
			continue
		}
		schemas = append(schemas, schema)
	}
	return schemas
}

// GetTools returns the tools the LLM may call. When session skills are
// included, the read tool is exposed so the LLM can resolve skill URIs.
// It mirrors AgentTrajectoryContextProvider.get_tools.
func (p *AgentTrajectoryContextProvider) GetTools() []string {
	if p.IncludeSessionSkills {
		return []string{"read"}
	}
	return nil
}

// Prefetch returns the messages to feed the LLM. When session skills are
// disabled, only the conversation message is returned. It mirrors
// AgentTrajectoryContextProvider.prefetch.
func (p *AgentTrajectoryContextProvider) Prefetch(ctx context.Context) ([]map[string]any, error) {
	if !p.IncludeSessionSkills {
		if !p.hasMessages() {
			return nil, nil
		}
		return []map[string]any{p.buildConversationMessage()}, nil
	}
	// When session skills are enabled, defer to the base provider's
	// prefetch (which loads read/search results).
	return p.SessionExtractContextProvider.Prefetch(ctx)
}

// ExecuteTool runs one tool call. When session skills are enabled, the
// base provider's ExecuteTool is used; otherwise the call is rejected.
// It mirrors AgentTrajectoryContextProvider.execute_tool.
func (p *AgentTrajectoryContextProvider) ExecuteTool(ctx context.Context, call ToolCall) (any, error) {
	if !p.IncludeSessionSkills {
		return map[string]any{"error": fmt.Sprintf("Unknown tool: %s", call.Name)}, nil
	}
	return p.SessionExtractContextProvider.ExecuteTool(ctx, call)
}

// SessionSkillMemoryType is the memory type for session-scoped skills.
// It mirrors openviking.session.skill.session_skill_context_provider.SESSION_SKILL_MEMORY_TYPE.
const SessionSkillMemoryType = "session_skills"

// sharedSkillState is the set of attributes shared between the
// trajectory provider and its embedded skill provider. It mirrors
// AgentTrajectoryContextProvider._SHARED_SKILL_STATE.
var sharedSkillState = map[string]struct{}{
	"messages":                  {},
	"latest_archive_overview":   {},
	"_output_language":          {},
	"_extract_context":          {},
	"_isolation_handler":        {},
	"_read_file_contents":       {},
	"_ctx":                      {},
	"_viking_fs":                {},
	"_transaction_handle":       {},
}

// SharedSkillStateKeys returns the keys of the shared skill-state set,
// sorted, for use in tests and diagnostics.
func SharedSkillStateKeys() []string {
	out := make([]string, 0, len(sharedSkillState))
	for k := range sharedSkillState {
		out = append(out, k)
	}
	return sortStrings(out)
}

// sortStrings returns a sorted copy of in.
func sortStrings(in []string) []string {
	out := make([]string, len(in))
	copy(out, in)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// _ = strings.Join keeps the import active when the shared-state set is
// the only consumer. Remove once a real consumer lands.
var _ = strings.Join
