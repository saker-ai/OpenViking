// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// PrefetchSearchQueryMaxChars caps the semantic-search query length used
// during prefetch. It mirrors
// session_extract_context_provider._PREFETCH_SEARCH_QUERY_MAX_CHARS.
const PrefetchSearchQueryMaxChars = 5000

// PrefetchSearchTextPartMaxChars caps one user text part's contribution
// to the prefetch search query.
const PrefetchSearchTextPartMaxChars = 1000

// PrefetchSearchAssistantTextPartMaxChars caps one assistant text part's
// contribution to the prefetch search query.
const PrefetchSearchAssistantTextPartMaxChars = 500

// SessionExtractContextProvider is the base provider for extracting
// user memories from a conversation. It mirrors
// openviking.session.memory.session_extract_context_provider.SessionExtractContextProvider.
//
// Subclasses (AgentExperienceContextProvider, AgentTrajectoryContextProvider,
// PatchMergeContextProvider) override Instruction, GetMemorySchemas,
// GetTools, and Prefetch to customize behavior.
type SessionExtractContextProvider struct {
	Messages             []Message
	LatestArchiveOverview string
	OutputLanguage       string

	registry          *MemoryTypeRegistry
	extractContext    *ExtractContext
	isolationHandler  *MemoryIsolationHandler
	readFileContents  map[string]MemoryFile
	ctx               *IdentityContext
	vikingFS          MemoryFS
	transactionHandle any
	eagerPrefetch     bool
	prefetchSearchTopN int
	linkEnabled       bool
	includeToolParts  bool
}

// NewSessionExtractContextProvider constructs a provider. The messages
// slice is copied so the caller can safely mutate the original.
func NewSessionExtractContextProvider(messages []Message, latestArchiveOverview string) *SessionExtractContextProvider {
	p := &SessionExtractContextProvider{
		LatestArchiveOverview: latestArchiveOverview,
		readFileContents:      make(map[string]MemoryFile),
		prefetchSearchTopN:    5,
	}
	p.Messages = make([]Message, len(messages))
	copy(p.Messages, messages)
	p.OutputLanguage = p.detectLanguage()
	return p
}

// SetIsolationHandler attaches an isolation handler. Required before
// Prefetch when peer-scoped reads are needed.
func (p *SessionExtractContextProvider) SetIsolationHandler(ih *MemoryIsolationHandler) {
	p.isolationHandler = ih
}

// SetVikingFS attaches the filesystem used for read/search during prefetch.
func (p *SessionExtractContextProvider) SetVikingFS(fs MemoryFS) {
	p.vikingFS = fs
}

// SetContext attaches the request context (user/account).
func (p *SessionExtractContextProvider) SetContext(ctx *IdentityContext) {
	p.ctx = ctx
}

// SetTransactionHandle attaches the transaction handle used for locking.
func (p *SessionExtractContextProvider) SetTransactionHandle(h any) {
	p.transactionHandle = h
}

// SetEagerPrefetch configures whether prefetch reads the top-N search
// results in addition to listing.
func (p *SessionExtractContextProvider) SetEagerPrefetch(b bool) {
	p.eagerPrefetch = b
}

// SetLinkEnabled configures whether the structured operations schema
// includes a "links" field.
func (p *SessionExtractContextProvider) SetLinkEnabled(b bool) {
	p.linkEnabled = b
}

// SetIncludeToolParts configures whether tool-call evidence is included
// in the conversation text. Defaults to false (user-scope extraction
// omits tool evidence).
func (p *SessionExtractContextProvider) SetIncludeToolParts(b bool) {
	p.includeToolParts = b
}

// ReadFileContents returns the URI -> MemoryFile cache populated by
// prefetch and read. It mirrors the read_file_contents property.
func (p *SessionExtractContextProvider) ReadFileContents() map[string]MemoryFile {
	return p.readFileContents
}

// GetExtractContext returns the cached ExtractContext, building one on
// first access. It mirrors SessionExtractContextProvider.get_extract_context.
func (p *SessionExtractContextProvider) GetExtractContext() *ExtractContext {
	if p.extractContext == nil {
		p.extractContext = NewExtractContext(p.Messages, true)
	}
	return p.extractContext
}

// SetExtractContext overrides the cached ExtractContext. Used by
// PatchMergeContextProvider consumers (e.g. StreamingMemoryUpdater) that
// need to inject a pre-built context rather than derive one from messages.
func (p *SessionExtractContextProvider) SetExtractContext(ec *ExtractContext) {
	p.extractContext = ec
}

// GetOutputLanguage returns the detected output language.
func (p *SessionExtractContextProvider) GetOutputLanguage() string {
	return p.OutputLanguage
}

// Instruction returns the system prompt. It mirrors
// SessionExtractContextProvider.instruction.
func (p *SessionExtractContextProvider) Instruction() string {
	return fmt.Sprintf(`You are a memory extraction agent. Your task is to analyze conversations and update memories.

## Workflow
1. Analyze the conversation and pre-fetched context
2. If you need more information, use the available tools (read/search)
3. When you have enough information, output ONLY a JSON object (no extra text before or after)

## Critical
- ONLY read and search tools are available - DO NOT use write tool
- Before editing ANY existing memory file, you MUST first read its complete content
- ONLY read URIs that are explicitly listed in ls/search tool results, returned by previous tool calls

## Target Output Language
All memory content MUST be written in %s.

## URI Handling
The system automatically generates URIs based on memory_type and fields. Just provide correct memory_type and fields.

## Self and Peer Memory
When a memory item describes the current user, omit peer_id.
When a memory item describes a peer, set peer_id to one of the peer_id values allowed by
the output schema. Do not invent peer_id values.
For events with ranges, the system derives self/peer targets from the message range.
Message role is authoritative: user-role content is the source for profile/preferences/entities/events,
and assistant-role content is the source for cases/patterns/tools/skills. Do not infer ownership
from neighboring messages.
`, p.OutputLanguage)
}

// GetTools returns the tools the LLM may call. When eager_prefetch is
// on, prefetch already loaded everything so no tools are needed.
func (p *SessionExtractContextProvider) GetTools() []string {
	if p.eagerPrefetch {
		return nil
	}
	return []string{"read"}
}

// GetMemorySchemas returns the user-stage schemas the LLM should
// produce operations for. It mirrors
// SessionExtractContextProvider.get_memory_schemas.
func (p *SessionExtractContextProvider) GetMemorySchemas(requestCtx any) []MemoryTypeSchema {
	registry := p.getRegistry()
	schemas := make([]MemoryTypeSchema, 0, 8)
	for _, mt := range registry.ListAll(false) {
		if mt.Stage == "" || mt.Stage == "user" {
			schemas = append(schemas, mt)
		}
	}
	if p.isolationHandler != nil {
		out := make([]MemoryTypeSchema, 0, len(schemas))
		for _, s := range schemas {
			if p.isolationHandler.AllowsSchema(s) {
				out = append(out, s)
			}
		}
		return out
	}
	return schemas
}

// Prefetch returns the messages to feed the LLM. The first message is
// the conversation history; subsequent messages are read/search results.
// It mirrors SessionExtractContextProvider.prefetch.
func (p *SessionExtractContextProvider) Prefetch(ctx context.Context) ([]map[string]any, error) {
	if !p.hasMessages() {
		return nil, nil
	}
	out := make([]map[string]any, 0, 8)
	out = append(out, p.buildConversationMessage())
	schemas := p.GetMemorySchemas(p.ctx)
	roleScope := RoleScope{}
	if p.isolationHandler != nil {
		roleScope = p.isolationHandler.GetReadScope()
	}
	lsDirs := make(map[string]struct{}, 4)
	readFiles := make(map[string]struct{}, 4)
	for _, schema := range schemas {
		if schema.Directory == "" {
			continue
		}
		if schema.OperationMode == "add_only" {
			continue
		}
		schemaDirs := p.renderSchemaDirectories(schema, roleScope)
		if schema.FilenameHasVariables() {
			for _, d := range schemaDirs {
				lsDirs[d] = struct{}{}
			}
		} else {
			for _, d := range schemaDirs {
				readFiles[d+"/"+schema.FilenameTemplate] = struct{}{}
			}
		}
	}
	callIDSeq := 0
	if len(lsDirs) > 0 {
		dirList := make([]string, 0, len(lsDirs))
		for d := range lsDirs {
			dirList = append(dirList, d)
		}
		sort.Strings(dirList)
		query := p.buildPrefetchSearchQuery()
		if query == "" {
			query = "conversation"
		}
		searchURIs, _ := p.searchFiles(ctx, query, dirList, 5)
		out = AddToolCallPairToMessages(out, callIDSeq, "search",
			map[string]any{"query": "[Keywords]", "search_uri": dirList},
			searchURIs)
		callIDSeq++
		if p.eagerPrefetch {
			for _, uri := range searchURIs {
				if uri == "" {
					continue
				}
				callIDSeq = p.appendStructuredReadResult(ctx, out, callIDSeq, uri)
			}
		}
	}
	for fileURI := range readFiles {
		callIDSeq = p.appendStructuredReadResult(ctx, out, callIDSeq, fileURI)
	}
	return out, nil
}

// ExecuteTool runs one tool call via the default memory tools registry.
// It mirrors SessionExtractContextProvider.execute_tool.
func (p *SessionExtractContextProvider) ExecuteTool(ctx context.Context, call ToolCall) (any, error) {
	tool := DefaultMemoryTools.Get(call.Name)
	if tool == nil {
		return map[string]any{"error": fmt.Sprintf("Unknown tool: %s", call.Name)}, nil
	}
	tc := &ToolContext{
		VikingFS:         p.vikingFS,
		RequestContext:   p.ctx,
		ReadFileContents: p.readFileContents,
		PageIDMap:        p.GetExtractContext().PageIDMap,
	}
	return tool.Execute(ctx, tc, call.Arguments)
}

// hasMessages reports whether the provider has a non-empty message list.
func (p *SessionExtractContextProvider) hasMessages() bool {
	return len(p.Messages) > 0
}

// getRegistry returns the lazily-initialized registry.
func (p *SessionExtractContextProvider) getRegistry() *MemoryTypeRegistry {
	if p.registry == nil {
		p.registry = NewMemoryTypeRegistry()
	}
	return p.registry
}

// SetRegistry overrides the registry. Used by subclasses and tests.
func (p *SessionExtractContextProvider) SetRegistry(r *MemoryTypeRegistry) {
	p.registry = r
}

// detectLanguage returns the detected output language for the current
// messages. It mirrors SessionExtractContextProvider._detect_language.
func (p *SessionExtractContextProvider) detectLanguage() string {
	userTextParts := make([]string, 0, len(p.Messages))
	allTextParts := make([]string, 0, len(p.Messages))
	for _, msg := range p.Messages {
		for _, part := range msg.Parts() {
			text := part.Text()
			if text == "" {
				continue
			}
			allTextParts = append(allTextParts, text)
			if msg.Role() == "user" {
				userTextParts = append(userTextParts, text)
			}
		}
	}
	textParts := userTextParts
	if len(textParts) == 0 {
		textParts = allTextParts
	}
	if len(textParts) == 0 {
		return "en"
	}
	return ResolveOutputLanguage(strings.Join(textParts, "\n"))
}

// buildConversationMessage returns the user message that contains the
// conversation history. It mirrors
// SessionExtractContextProvider._build_conversation_message.
func (p *SessionExtractContextProvider) buildConversationMessage() map[string]any {
	var firstTime, lastTime string
	if len(p.Messages) > 0 {
		firstTime = p.Messages[0].CreatedAt()
		lastTime = p.Messages[len(p.Messages)-1].CreatedAt()
	}
	sessionTime := "now"
	dayOfWeek := ""
	if firstTime != "" {
		if t, err := time.Parse(time.RFC3339, firstTime); err == nil {
			sessionTime = t.Format("2006-01-02 15:04")
			dayOfWeek = t.Weekday().String()
		}
	}
	timeDisplay := sessionTime
	if lastTime != "" && lastTime != firstTime {
		if t, err := time.Parse(time.RFC3339, lastTime); err == nil {
			timeDisplay = sessionTime + " - " + t.Format("2006-01-02 15:04")
		}
	}
	conv := p.assembleConversation()
	header := fmt.Sprintf("## Conversation History\n**Session Time:** %s (%s)\nRelative times (e.g., 'last week', 'next month') are based on Session Time, not today.\n\n", timeDisplay, dayOfWeek)
	body := header + conv + "\n\nAfter exploring, analyze the conversation and output ALL memory write/edit/delete operations in a single response. Do not output operations one at a time - gather all changes first, then return them together."
	return map[string]any{"role": "user", "content": body}
}

// assembleConversation formats the message list as a numbered transcript.
// It mirrors SessionExtractContextProvider._assemble_conversation.
func (p *SessionExtractContextProvider) assembleConversation() string {
	formatted := make([]string, 0, len(p.Messages))
	for idx, msg := range p.Messages {
		body := p.formatMessageParts(msg)
		if strings.TrimSpace(body) == "" {
			continue
		}
		speaker := msg.PeerID()
		if speaker == "" {
			speaker = msg.Role()
		}
		formatted = append(formatted, fmt.Sprintf("[%d][%s][%s]: %s", idx, msg.Role(), speaker, body))
	}
	return strings.Join(formatted, "\n")
}

// formatMessageParts concatenates the text parts of one message. When
// includeToolParts is set, tool-call evidence is appended as well.
func (p *SessionExtractContextProvider) formatMessageParts(msg Message) string {
	parts := make([]string, 0, len(msg.Parts()))
	for _, part := range msg.Parts() {
		if t := part.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

// buildPrefetchSearchQuery returns a compact semantic-search query built
// from the raw message text. It mirrors
// SessionExtractContextProvider._build_prefetch_search_query.
func (p *SessionExtractContextProvider) buildPrefetchSearchQuery() string {
	if len(p.Messages) == 0 {
		return ""
	}
	primary := make([]string, 0, len(p.Messages))
	supporting := make([]string, 0, len(p.Messages))
	for _, msg := range p.Messages {
		role := msg.Role()
		speaker := msg.PeerID()
		if speaker == "" {
			speaker = role
		}
		textParts := make([]string, 0, 4)
		for _, part := range msg.Parts() {
			t := part.Text()
			if t == "" {
				continue
			}
			limit := PrefetchSearchTextPartMaxChars
			if role != "user" {
				limit = PrefetchSearchAssistantTextPartMaxChars
			}
			textParts = append(textParts, TruncateContent(t, limit))
		}
		if len(textParts) == 0 {
			continue
		}
		section := speaker + ": " + strings.Join(textParts, "\n")
		if role == "user" {
			primary = append(primary, section)
		} else {
			supporting = append(supporting, section)
		}
	}
	query := strings.Join(primary, "\n\n")
	if len(supporting) > 0 {
		if query != "" {
			query += "\n\n"
		}
		query += strings.Join(supporting, "\n\n")
	}
	if strings.TrimSpace(query) == "" {
		return p.assembleConversation()
	}
	return TruncateContent(query, PrefetchSearchQueryMaxChars)
}

// renderSchemaDirectories returns the directory URIs for one schema
// under the current isolation scope. It mirrors the directory-rendering
// branch of prefetch.
func (p *SessionExtractContextProvider) renderSchemaDirectories(schema MemoryTypeSchema, roleScope RoleScope) []string {
	if p.isolationHandler != nil {
		return p.isolationHandler.RenderSchemaDirectories(schema)
	}
	if roleScope.UserID == "" {
		return nil
	}
	rendered, err := RenderTemplate(schema.Directory, map[string]any{"user_space": roleScope.UserID})
	if err != nil {
		return nil
	}
	return []string{rendered}
}

// searchFiles runs a semantic search via the search tool. Returns a
// list of URIs. It mirrors SessionExtractContextProvider.search_files.
func (p *SessionExtractContextProvider) searchFiles(ctx context.Context, query string, searchURIs []string, limit int) ([]string, error) {
	if p.vikingFS == nil || query == "" {
		return nil, nil
	}
	tool := DefaultMemoryTools.Get("search")
	if tool == nil {
		return nil, nil
	}
	tc := &ToolContext{
		VikingFS:         p.vikingFS,
		RequestContext:   p.ctx,
		ReadFileContents: p.readFileContents,
		PageIDMap:        p.GetExtractContext().PageIDMap,
		DefaultSearchURIs: strings.Join(searchURIs, ","),
	}
	result, err := tool.Execute(ctx, tc, map[string]any{"query": query, "limit": limit})
	if err != nil {
		return nil, err
	}
	return extractURIsFromSearchResult(result, limit), nil
}

// appendStructuredReadResult reads one URI and appends a read tool-call
// pair to messages. Returns the next call_id. It mirrors
// SessionExtractContextProvider._append_structured_read_result.
func (p *SessionExtractContextProvider) appendStructuredReadResult(ctx context.Context, messages []map[string]any, callID int, uri string) int {
	result, err := p.readFile(ctx, uri)
	if err != nil || result == nil {
		return callID
	}
	AddToolCallPairToMessages(messages, callID, "read", map[string]any{"uri": uri}, result)
	return callID + 1
}

// readFile reads one URI via the read tool. It mirrors
// SessionExtractContextProvider.read_file.
func (p *SessionExtractContextProvider) readFile(ctx context.Context, uri string) (any, error) {
	tool := DefaultMemoryTools.Get("read")
	if tool == nil {
		return nil, nil
	}
	tc := &ToolContext{
		VikingFS:         p.vikingFS,
		RequestContext:   p.ctx,
		ReadFileContents: p.readFileContents,
		PageIDMap:        p.GetExtractContext().PageIDMap,
	}
	result, err := tool.Execute(ctx, tc, map[string]any{"uri": uri})
	if err != nil {
		return nil, err
	}
	if m, ok := result.(map[string]any); ok {
		if _, hasErr := m["error"]; hasErr {
			return nil, nil
		}
	}
	return result, nil
}

// extractURIsFromSearchResult pulls URIs out of the search tool result.
func extractURIsFromSearchResult(result any, limit int) []string {
	switch x := result.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				if uri, _ := m["uri"].(string); uri != "" {
					out = append(out, uri)
				}
			}
		}
		return out
	case map[string]any:
		if memories, ok := x["memories"].([]any); ok {
			out := make([]string, 0, len(memories))
			for _, item := range memories {
				if m, ok := item.(map[string]any); ok {
					if uri, _ := m["uri"].(string); uri != "" {
						out = append(out, uri)
					}
				}
			}
			return out
		}
	}
	return nil
}
