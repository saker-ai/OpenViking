// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"fmt"
	"strings"
)

// ExperienceMemoryType is defined in constants.go and is reused here.
// ExperienceSearchTopK is the maximum number of candidate experiences the
// provider prefetches.
const ExperienceSearchTopK = 5

// ExperienceSourceTrajTopK is the number of top candidates whose source
// trajectories are also prefetched.
const ExperienceSourceTrajTopK = 3

// ExperienceMaxSourceTrajs is the maximum number of source trajectories
// loaded per candidate experience.
const ExperienceMaxSourceTrajs = 3

// AgentExperienceContextProvider is Phase 2 of agent-scope extraction.
// Given a new trajectory summary, it searches for candidate experiences
// and lets the LLM decide whether to update, replace, create, or skip.
// It mirrors openviking.session.memory.agent_experience_context_provider.AgentExperienceContextProvider.
type AgentExperienceContextProvider struct {
	*SessionExtractContextProvider

	TrajectorySummary string
	TrajectoryURI     string
	PrefetchedURIs   []string
}

// NewAgentExperienceContextProvider constructs the provider. It mirrors
// AgentExperienceContextProvider.__init__.
func NewAgentExperienceContextProvider(
	messages []Message,
	trajectorySummary, trajectoryURI, latestArchiveOverview string,
) *AgentExperienceContextProvider {
	return &AgentExperienceContextProvider{
		SessionExtractContextProvider: NewSessionExtractContextProvider(messages, latestArchiveOverview),
		TrajectorySummary:             trajectorySummary,
		TrajectoryURI:                 trajectoryURI,
		PrefetchedURIs:                make([]string, 0, ExperienceSearchTopK),
	}
}

// Instruction returns the system prompt. It mirrors
// AgentExperienceContextProvider.instruction.
func (p *AgentExperienceContextProvider) Instruction() string {
	return fmt.Sprintf(`You are a memory extraction agent. Your job is to distill experience memories from agent execution trajectories.

You are given:
- A new trajectory (the latest agent execution to incorporate)
- Up to %d candidate existing experiences (retrieved by relevance). Top candidates also include their source trajectories as grounding material.

The source trajectories are for reference only — do NOT include or modify them in your output.

## What to output

For each distinct user intent in the trajectory, output a SEPARATE experience entry. A single trajectory may contain multiple user intents — you MUST produce one entry per intent, not one entry for the whole trajectory.

Each entry:
- ` + "`experience_name`" + `: the name of the experience (new or existing)
- ` + "`content`" + `: the full experience content (rewrite holistically, incorporating old + new)
- ` + "`supersedes`" + `: the ` + "`experience_name`" + ` of an older experience this one replaces — set ONLY when the new name is genuinely different and broader. Leave empty otherwise.

The system handles create vs update automatically:
- Same ` + "`experience_name`" + ` as an existing one → updates it in place
- New ` + "`experience_name`" + ` → creates a new experience
- ` + "`supersedes`" + ` set → old experience is deleted and its history is inherited

## Rules

- **One experience per distinct user intent.** If a trajectory covers N different user goals (e.g., cancel + modify + add baggage), output N separate entries — never merge them into one.
- **Split over merge.** When in doubt whether two patterns belong together, split them. Only merge with an existing experience when it covers the EXACT same user intent and tool sequence.
- **Consistent naming language.** All ` + "`experience_name`" + ` values in one output must use the same language.
- **Do NOT use ` + "`delete_ids`" + `** for experience operations — use ` + "`supersedes`" + ` instead.
- Follow field descriptions in the schema.
- Output JSON only. Do not call any tools.

All memory content must be written in %s.
`, ExperienceSearchTopK, p.OutputLanguage)
}

// GetMemorySchemas returns the experience schema. It mirrors
// AgentExperienceContextProvider.get_memory_schemas.
func (p *AgentExperienceContextProvider) GetMemorySchemas(requestCtx any) []MemoryTypeSchema {
	registry := p.getRegistry()
	schema, ok := registry.Get(ExperienceMemoryType)
	if !ok || !schema.Enabled {
		return nil
	}
	return []MemoryTypeSchema{schema}
}

// GetTools returns no tools — all context is prefetched. It mirrors
// AgentExperienceContextProvider.get_tools.
func (p *AgentExperienceContextProvider) GetTools() []string {
	return nil
}

// Prefetch returns the messages to feed the LLM. The first message is
// the conversation; subsequent messages are read results for the new
// trajectory and candidate experiences (plus their source trajectories).
// It mirrors AgentExperienceContextProvider.prefetch.
func (p *AgentExperienceContextProvider) Prefetch(ctx context.Context) ([]map[string]any, error) {
	if !p.hasMessages() {
		return nil, nil
	}
	experienceDir := p.renderExperienceDir()
	candidateURIs := make([]string, 0, ExperienceSearchTopK)
	if experienceDir != "" && p.vikingFS != nil {
		query := p.TrajectorySummary
		if len(query) > 500 {
			query = query[:500]
		}
		if query == "" {
			query = "experience"
		}
		uris, _ := p.searchFiles(ctx, query, []string{experienceDir}, ExperienceSearchTopK)
		candidateURIs = uris
		if len(candidateURIs) == 0 {
			candidateURIs = p.fallbackListExperiences(ctx, experienceDir)
		}
	}
	out := make([]map[string]any, 0, 8)
	out = append(out, p.buildConversationMessage())
	out = AddToolCallPairToMessages(out, "new-trajectory", "read",
		map[string]any{"uri": p.TrajectoryURI},
		map[string]any{
			"memory_type":  "trajectories",
			"content":      p.TrajectorySummary,
			"uri":          p.TrajectoryURI,
			"context_role": "new_trajectory",
		})
	callIDSeq := 0
	for idx, expURI := range candidateURIs {
		result, _ := p.readFile(ctx, expURI)
		if result == nil {
			continue
		}
		p.PrefetchedURIs = append(p.PrefetchedURIs, expURI)
		mf, ok := p.readFileContents[expURI]
		if !ok {
			continue
		}
		if m, ok := result.(map[string]any); ok {
			m["context_role"] = "candidate_experience"
		}
		out = AddToolCallPairToMessages(out, callIDSeq, "read",
			map[string]any{"uri": expURI}, result)
		callIDSeq++
		if idx < ExperienceSourceTrajTopK && p.vikingFS != nil {
			sourceTrajs := p.loadSourceTrajectories(ctx, expURI, mf.Links)
			for sourceIdx, sourceResult := range sourceTrajs {
				sourceURI, _ := sourceResult["uri"].(string)
				out = AddToolCallPairToMessages(out,
					fmt.Sprintf("source-%d-%d", idx, sourceIdx), "read",
					map[string]any{"uri": sourceURI}, sourceResult)
			}
		}
	}
	out = append(out, map[string]any{
		"role": "user",
		"content": strings.Join([]string{
			"You have already read the conversation, one `new_trajectory`, candidate experience memories, and optional `candidate_source_trajectory` references.",
			"Treat `new_trajectory` as the new execution to incorporate.",
			"Treat `candidate_experience` as existing memories you may update, replace, or skip.",
			"Treat `candidate_source_trajectory` as reference-only context for understanding a candidate experience; do not modify it directly.",
			"Based on the above, decide whether to **Update**, **Replace**, **Create**, or **Skip**. Output JSON only.",
			"A single trajectory covering multiple user intents MUST produce multiple entries.",
		}, "\n"),
	})
	return out, nil
}

// renderExperienceDir returns the directory URI for the experience schema.
// It mirrors AgentExperienceContextProvider._render_experience_dir.
func (p *AgentExperienceContextProvider) renderExperienceDir() string {
	registry := p.getRegistry()
	schema, ok := registry.Get(ExperienceMemoryType)
	if !ok || schema.Directory == "" {
		return ""
	}
	userSpace := "default"
	if p.ctx != nil && p.ctx.UserID != "" {
		userSpace = p.ctx.UserID
	}
	rendered, err := RenderTemplate(schema.Directory, map[string]any{"user_space": userSpace})
	if err != nil {
		return ""
	}
	return rendered
}

// fallbackListExperiences lists the experience directory directly when
// semantic search returns no results. It mirrors the fallback branch
// of AgentExperienceContextProvider.prefetch.
func (p *AgentExperienceContextProvider) fallbackListExperiences(ctx context.Context, experienceDir string) []string {
	if p.vikingFS == nil {
		return nil
	}
	entries, err := p.vikingFS.Ls(ctx, experienceDir, "original", 256, false, 1000, p.ctx)
	if err != nil {
		return nil
	}
	out := make([]string, 0, ExperienceSearchTopK)
	for _, e := range entries {
		uri, _ := e["uri"].(string)
		name, _ := e["name"].(string)
		if uri == "" {
			uri = experienceDir + "/" + name
		}
		if !strings.HasSuffix(uri, ".md") {
			continue
		}
		if name == ".overview.md" || name == ".abstract.md" {
			continue
		}
		if strings.HasSuffix(uri, "/.overview.md") || strings.HasSuffix(uri, "/.abstract.md") {
			continue
		}
		out = append(out, uri)
		if len(out) >= ExperienceSearchTopK {
			break
		}
	}
	return out
}

// loadSourceTrajectories reads the most recent source-trajectory URIs
// from the candidate experience's links. It mirrors
// AgentExperienceContextProvider._load_source_trajectories.
func (p *AgentExperienceContextProvider) loadSourceTrajectories(ctx context.Context, expURI string, links []map[string]any) []map[string]any {
	if p.vikingFS == nil {
		return nil
	}
	uris := make([]string, 0, ExperienceMaxSourceTrajs)
	for _, link := range links {
		linkType, _ := link["link_type"].(string)
		toURI, _ := link["to_uri"].(string)
		if linkType == "derived_from" && toURI != "" {
			uris = append(uris, toURI)
		}
	}
	if len(uris) > ExperienceMaxSourceTrajs {
		uris = uris[len(uris)-ExperienceMaxSourceTrajs:]
	}
	results := make([]map[string]any, 0, len(uris))
	for _, uri := range uris {
		raw, err := p.vikingFS.ReadFile(ctx, uri, p.ctx)
		if err != nil || raw == "" {
			continue
		}
		mf := MemoryFileRead(raw, uri)
		result := mf.ToMetadata()
		result["content"] = mf.Content
		result["uri"] = uri
		results = append(results, result)
	}
	return results
}
