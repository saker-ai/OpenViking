// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// VLMResponse is the response from a VLM call. It mirrors the subset of
// openviking.models.vlm.base.VLMResponse used by the extract loop.
type VLMResponse struct {
	Content   string
	ToolCalls []ToolCall
	Usage     map[string]any
}

// HasToolCalls reports whether the response carries tool calls.
func (r *VLMResponse) HasToolCalls() bool {
	return r != nil && len(r.ToolCalls) > 0
}

// ToolCall is one LLM-emitted tool call. It mirrors
// openviking.models.vlm.base.ToolCall.
type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

// ExtractVLM is the minimal interface a vision-language model must
// satisfy for the extract loop. It mirrors the subset of
// openviking.models.vlm.base.VLMBase used by ExtractLoop.
//
// This is distinct from the simpler VLM interface in vision_normalizer.go
// (which only describes images). The extract loop needs richer
// tool-calling support, so it uses ExtractVLM instead.
type ExtractVLM interface {
	// GetCompletionAsync sends messages to the LLM and returns the
	// response. tools is the OpenAI function-schema list (or nil when
	// no tools should be exposed). toolChoice is "auto" or "".
	GetCompletionAsync(ctx context.Context, messages []map[string]any, tools []map[string]any, toolChoice string, thinking bool) (*VLMResponse, error)
}

// ContextProvider is the abstract base for extraction context providers.
// It mirrors openviking.session.memory.core.ExtractContextProvider.
//
// Implementations include SessionExtractContextProvider,
// AgentExperienceContextProvider, AgentTrajectoryContextProvider, and
// PatchMergeContextProvider.
type ContextProvider interface {
	// Instruction returns the system prompt for the LLM.
	Instruction() string
	// Prefetch returns the messages to feed the LLM before the first
	// iteration (conversation history + read/search results).
	Prefetch(ctx context.Context) ([]map[string]any, error)
	// GetTools returns the names of tools the LLM may call.
	GetTools() []string
	// GetMemorySchemas returns the schemas the LLM should produce
	// operations for.
	GetMemorySchemas(requestCtx any) []MemoryTypeSchema
	// GetOutputLanguage returns the language memory content should be
	// written in.
	GetOutputLanguage() string
	// GetExtractContext returns the ExtractContext (messages + page_id_map).
	GetExtractContext() *ExtractContext
	// ExecuteTool runs one tool call. Returns the tool result or an
	// error-shaped map.
	ExecuteTool(ctx context.Context, call ToolCall) (any, error)
	// ReadFileContents returns the URI -> MemoryFile cache populated by
	// prefetch/read.
	ReadFileContents() map[string]MemoryFile
}

// cannedRefusalRE matches generic LLM refusals like "sorry, I can't".
var cannedRefusalRE = regexp.MustCompile(`(?i)(抱歉|不好意思|很遗憾|sorry).{0,20}(无法|不能|未能|不给|没有找到|未找到|can't|cannot|unable|won't|not able)`)

// cannedRefusalPhrases is the set of literal refusal phrases the loop
// treats as "the LLM refused to answer".
var cannedRefusalPhrases = []string{
	"您的问题我无法回答",
	"您的问题我无法识别",
	"我无法回答这个问题",
	"我无法给到相关内容",
	"这个问题未找到相关结果",
	"没有找到相关的结果",
	"I can't answer that",
	"I cannot answer that",
	"I can't help with that",
	"I cannot help with that",
}

// LooksLikeCannedRefusal reports whether a non-JSON LLM response looks
// like a generic refusal. It mirrors _looks_like_canned_refusal.
func LooksLikeCannedRefusal(content string) bool {
	text := strings.Join(strings.Fields(content), " ")
	if text == "" {
		return false
	}
	if cannedRefusalRE.MatchString(text) {
		return true
	}
	for _, phrase := range cannedRefusalPhrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

// PreviewText truncates content to limit characters, collapsing
// whitespace first. It mirrors _preview_text.
func PreviewText(content string, limit int) string {
	if limit <= 0 {
		limit = 240
	}
	text := strings.Join(strings.Fields(content), " ")
	if len(text) <= limit {
		return text
	}
	return text[:limit-3] + "..."
}

// ExtractLoop is the simplified ReAct orchestrator for memory updates.
// It mirrors openviking.session.memory.extract_loop.ExtractLoop.
//
// Workflow:
//  1. Pre-fetch context via the provider.
//  2. Call LLM with tools; model either calls tools or outputs final
//     operations.
//  3. When tools are called, execute them and continue the loop.
//  4. When operations are output, resolve and return them.
type ExtractLoop struct {
	VLM             ExtractVLM
	FS              MemoryFS
	Model           string
	MaxIterations   int
	Ctx             any
	ContextProvider ContextProvider
	IsolationHandler *MemoryIsolationHandler
	Thinking        bool

	schemaGenerator   *SchemaModelGenerator
	toolSchemas       []map[string]any
	expectedFields    []string
	operationsModel   map[string]any
	linkEnabled       bool
	formatRetryCount  int
	lastFailureKind   string
	lastFailureContent string
	disableTools      bool
}

// NewExtractLoop constructs an ExtractLoop. maxIterations defaults to 3
// when <= 0. The context provider must be set before Run is called.
func NewExtractLoop(
	vlm ExtractVLM,
	fs MemoryFS,
	model string,
	maxIterations int,
	ctx any,
	provider ContextProvider,
	ih *MemoryIsolationHandler,
	thinking bool,
) *ExtractLoop {
	if maxIterations <= 0 {
		maxIterations = 3
	}
	return &ExtractLoop{
		VLM:             vlm,
		FS:              fs,
		Model:           model,
		MaxIterations:   maxIterations,
		Ctx:             ctx,
		ContextProvider: provider,
		IsolationHandler: ih,
		Thinking:        thinking,
	}
}

// Run executes the ReAct loop. It mirrors ExtractLoop.run.
//
// Returns the resolved operations and the list of tools used. When the
// LLM cannot produce parseable operations after maxIterations, the
// returned ResolvedOperations carries an error string in Errors.
func (l *ExtractLoop) Run(ctx context.Context) (*ResolvedOperations, []map[string]any, error) {
	if l.ContextProvider == nil {
		return nil, nil, errors.New("context provider is required")
	}
	schemas := l.ContextProvider.GetMemorySchemas(l.Ctx)
	outputLanguage := l.ContextProvider.GetOutputLanguage()
	l.schemaGenerator = NewSchemaModelGenerator(schemas, map[string]any{"language": outputLanguage})
	l.schemaGenerator.GenerateAllModels()

	allowedTools := l.ContextProvider.GetTools()
	l.toolSchemas = l.buildToolSchemas(allowedTools)

	l.linkEnabled = false
	l.expectedFields = []string{}
	if l.linkEnabled {
		l.expectedFields = append(l.expectedFields, "links")
	}
	for _, schema := range schemas {
		l.expectedFields = append(l.expectedFields, schema.MemoryType)
	}

	var roleScope *RoleScope
	if l.IsolationHandler != nil {
		rs := l.IsolationHandler.GetReadScope()
		roleScope = &rs
	}
	l.operationsModel = l.schemaGenerator.GetLLMJSONSchema(roleScope)

	schemaBytes, _ := json.Marshal(l.operationsModel)
	schemaStr := string(schemaBytes)

	systemMsg := map[string]any{
		"role": "system",
		"content": fmt.Sprintf(`%s

## Page ID Rules
- Every memory item you create or edit MUST include "page_id".
- For existing items, use the page_id shown in read/search results.
- For new items, assign a unique page_id >= 100.
- When editing an existing item, reuse its existing page_id.
- To delete an existing item, add an entry to ` + "`delete_ids`" + ` using its page_id.
- For canonical merges, set ` + "`replacement_page_id`" + ` to the surviving page that should inherit the deleted page's existing links/backlinks; for pure deletes, set ` + "`replacement_page_id`" + ` to null.

## Output Format
The final output of the model must strictly follow the JSON Schema format shown below:
` + "```json" + `
%s
` + "```",
			l.ContextProvider.Instruction(), schemaStr),
	}

	prefetch, err := l.ContextProvider.Prefetch(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("prefetch: %w", err)
	}
	messages := make([]map[string]any, 0, len(prefetch)+1)
	messages = append(messages, systemMsg)
	messages = append(messages, prefetch...)

	ec := l.ContextProvider.GetExtractContext()
	if ec == nil {
		return nil, nil, errors.New("extract context is required")
	}
	for uri := range l.ContextProvider.ReadFileContents() {
		ec.PageIDMap.GetPageID(uri)
	}

	toolsUsed := make([]map[string]any, 0)
	var finalOperations *ResolvedOperations
	var rawLinks []map[string]any

	iteration := 0
	maxIter := l.MaxIterations
	for iteration < maxIter {
		iteration++
		isLast := iteration >= maxIter
		if isLast {
			messages = append(messages, map[string]any{
				"role":    "user",
				"content": l.buildFinalOperationsInstruction(),
			})
		}
		toolCalls, operations, err := l.callLLM(ctx, messages)
		if err != nil {
			return nil, nil, err
		}
		if len(toolCalls) > 0 {
			hasUnknown, err := l.executeToolCalls(ctx, messages, toolCalls, toolsUsed)
			if err != nil {
				return nil, nil, err
			}
			if hasUnknown {
				l.disableTools = true
			}
			if iteration >= maxIter {
				maxIter++
				l.disableTools = true
			}
			continue
		}
		if operations != nil {
			resolved, links := l.ResolveOperations(operations)
			finalOperations = resolved
			rawLinks = links
			refetchURIs := l.checkUnreadExistingFiles(ctx, resolved)
			if len(refetchURIs) > 0 {
				l.addRefetchResultsToMessages(messages, refetchURIs)
				if iteration >= maxIter {
					maxIter++
				}
				continue
			}
			break
		}
		// No tool calls and no parseable operations.
		failureKind := l.lastFailureKind
		if failureKind == "" {
			failureKind = "unknown"
		}
		if l.formatRetryCount == 0 {
			l.formatRetryCount++
			maxIter++
			l.addFormatErrorMessage(messages)
		}
		if iteration >= maxIter {
			finalOperations = &ResolvedOperations{
				Errors: []string{fmt.Sprintf("Final response could not be parsed as JSON operations after %d iterations (failure_kind=%s)", maxIter, failureKind)},
			}
			break
		}
		l.disableTools = true
		continue
	}
	if finalOperations == nil {
		return nil, nil, fmt.Errorf("reached %d iterations without completion", maxIter)
	}
	if err := l.FinalizeOperations(finalOperations, rawLinks); err != nil {
		return nil, nil, err
	}
	return finalOperations, toolsUsed, nil
}

// buildToolSchemas returns the OpenAI function schemas for the allowed
// tool names. It mirrors the tool schema precomputation in ExtractLoop.run.
func (l *ExtractLoop) buildToolSchemas(allowed []string) []map[string]any {
	if len(allowed) == 0 {
		return nil
	}
	allow := make(map[string]struct{}, len(allowed))
	for _, n := range allowed {
		allow[n] = struct{}{}
	}
	registry := DefaultMemoryTools
	out := make([]map[string]any, 0, len(allowed))
	for _, name := range allowed {
		tool := registry.Get(name)
		if tool == nil {
			continue
		}
		if _, ok := allow[tool.Name()]; !ok {
			continue
		}
		out = append(out, tool.ToSchema())
	}
	return out
}

// callLLM calls the LLM with the current messages. Returns either tool
// calls or parsed operations (one is non-nil). It mirrors ExtractLoop._call_llm.
func (l *ExtractLoop) callLLM(ctx context.Context, messages []map[string]any) ([]ToolCall, map[string]any, error) {
	tools := l.toolSchemas
	toolChoice := ""
	if !l.disableTools && len(tools) > 0 {
		toolChoice = "auto"
	} else {
		tools = nil
	}
	resp, err := l.VLM.GetCompletionAsync(ctx, messages, tools, toolChoice, l.Thinking)
	if err != nil {
		return nil, nil, fmt.Errorf("llm call: %w", err)
	}
	l.lastFailureKind = ""
	l.lastFailureContent = ""
	if resp == nil {
		l.lastFailureKind = "empty_response"
		return nil, nil, nil
	}
	if resp.HasToolCalls() {
		return resp.ToolCalls, nil, nil
	}
	content := resp.Content
	if content == "" {
		l.lastFailureKind = "empty_response"
		l.lastFailureContent = ""
		return nil, nil, nil
	}
	operations, err := ParseJSONWithStability(content)
	if err != nil {
		failureKind := "parse_error"
		if LooksLikeCannedRefusal(content) {
			failureKind = "refusal_text"
		}
		l.lastFailureKind = failureKind
		l.lastFailureContent = content
		return nil, nil, nil
	}
	return nil, operations, nil
}

// executeToolCalls runs tool calls in order and appends results to
// messages. Returns true when any tool call referenced an unknown tool.
// It mirrors ExtractLoop._execute_tool_calls.
func (l *ExtractLoop) executeToolCalls(ctx context.Context, messages []map[string]any, toolCalls []ToolCall, toolsUsed []map[string]any) (bool, error) {
	hasUnknown := false
	for _, tc := range toolCalls {
		result, err := l.ContextProvider.ExecuteTool(ctx, tc)
		if err != nil {
			result = map[string]any{"error": err.Error()}
		}
		if m, ok := result.(map[string]any); ok {
			if errStr, _ := m["error"].(string); strings.HasPrefix(errStr, "Unknown tool:") {
				hasUnknown = true
			}
		}
		toolsUsed = append(toolsUsed, map[string]any{
			"tool_name": tc.Name,
			"params":    tc.Arguments,
			"result":    result,
		})
		messages = AddToolCallPairToMessages(messages, tc.ID, tc.Name, tc.Arguments, result)
	}
	return hasUnknown, nil
}

// ResolveOperations converts the LLM-output operations dict into
// ResolvedOperations. It mirrors ExtractLoop.resolve_operations.
//
// The operations map has per-memory-type keys (each a list of items)
// plus optional delete_ids and links. Each item is resolved to a
// ResolvedOperation with URIs bound via the page_id_map or the
// isolation handler.
func (l *ExtractLoop) ResolveOperations(operations map[string]any) (*ResolvedOperations, []map[string]any) {
	upsertOps := make([]ResolvedOperation, 0)
	deleteFiles := make([]MemoryFile, 0)
	errors := make([]string, 0)
	deleteReplacements := make(map[string]string)

	ec := l.ContextProvider.GetExtractContext()
	pageIDMap := ec.PageIDMap
	roleScope := RoleScope{}
	if l.IsolationHandler != nil {
		roleScope = l.IsolationHandler.GetReadScope()
	}
	for _, schema := range l.ContextProvider.GetMemorySchemas(l.Ctx) {
		value, ok := operations[schema.MemoryType]
		if !ok || value == nil {
			continue
		}
		items := toItemList(value)
		for _, item := range items {
			itemDict := make(map[string]any, len(item))
			for k, v := range item {
				itemDict[k] = v
			}
			itemDict["memory_type"] = schema.MemoryType
			if l.IsolationHandler != nil {
				l.IsolationHandler.FillIdentityFields(itemDict, roleScope, &schema)
			}
			pageID := 0
			if v, ok := itemDict["page_id"]; ok {
				if i, ok := asInt(v); ok {
					pageID = i
				}
				delete(itemDict, "page_id")
			}
			op := ResolvedOperation{
				MemoryFields: itemDict,
				MemoryType:   schema.MemoryType,
				URIs:         []string{},
				PageID:       nil,
			}
			if pageID != 0 {
				p := pageID
				op.PageID = &p
			}
			if op.PageID != nil && pageIDMap != nil {
				resolvedURI := pageIDMap.Resolve(*op.PageID)
				if resolvedURI != "" {
					op.URIs = []string{resolvedURI}
					if oldContent, ok := l.ContextProvider.ReadFileContents()[resolvedURI]; ok {
						op.OldMemoryFileContent = &oldContent
						immutableFields := make(map[string]struct{}, len(schema.Fields))
						for _, f := range schema.Fields {
							if f.MergeOp != MergeOpPatch {
								immutableFields[f.Name] = struct{}{}
							}
						}
						for fieldName := range immutableFields {
							if v, ok := oldContent.ExtraFields[fieldName]; ok {
								op.MemoryFields[fieldName] = v
							}
						}
					}
				} else if l.IsolationHandler != nil {
					op.URIs = l.IsolationHandler.CalculateMemoryURIs(schema, &op, ec)
				}
			} else if l.IsolationHandler != nil {
				op.URIs = l.IsolationHandler.CalculateMemoryURIs(schema, &op, ec)
			}
			upsertOps = append(upsertOps, op)
		}
	}
	// Delete IDs.
	deleteIDs := toDeleteIDs(operations["delete_ids"])
	for _, did := range deleteIDs {
		if did.DeletePageID == nil || pageIDMap == nil {
			continue
		}
		deleteURI := pageIDMap.Resolve(*did.DeletePageID)
		if deleteURI == "" {
			continue
		}
		oldContent, ok := l.ContextProvider.ReadFileContents()[deleteURI]
		if !ok {
			continue
		}
		deleteFiles = append(deleteFiles, oldContent)
		if did.ReplacementPageID == nil {
			continue
		}
		replacementURI := pageIDMap.Resolve(*did.ReplacementPageID)
		if replacementURI == "" {
			for _, op := range upsertOps {
				if op.PageID != nil && *op.PageID == *did.ReplacementPageID && len(op.URIs) > 0 {
					replacementURI = op.URIs[0]
					break
				}
			}
		}
		if replacementURI != "" && replacementURI != deleteURI {
			deleteReplacements[deleteURI] = replacementURI
		}
	}
	// Backfill OldMemoryFileContent for upserts that resolved a URI but
	// did not have it set during the page_id lookup.
	for i := range upsertOps {
		op := &upsertOps[i]
		if op.OldMemoryFileContent != nil {
			continue
		}
		for _, uri := range op.URIs {
			if oldContent, ok := l.ContextProvider.ReadFileContents()[uri]; ok {
				op.OldMemoryFileContent = &oldContent
				break
			}
		}
	}
	rawLinks := toRawLinks(operations["links"])
	resolved := &ResolvedOperations{
		UpsertOperations:   upsertOps,
		DeleteFileContents: deleteFiles,
		Errors:             errors,
		DeleteReplacements: deleteReplacements,
	}
	return resolved, rawLinks
}

// FinalizeOperations registers new page_ids and resolves links. It
// mirrors ExtractLoop.finalize_operations.
func (l *ExtractLoop) FinalizeOperations(operations *ResolvedOperations, rawLinks []map[string]any) error {
	if !l.linkEnabled || len(rawLinks) == 0 {
		return nil
	}
	ec := l.ContextProvider.GetExtractContext()
	if ec == nil || ec.PageIDMap == nil {
		return nil
	}
	for _, op := range operations.UpsertOperations {
		if op.PageID == nil || *op.PageID < 100 {
			continue
		}
		for _, uri := range op.URIs {
			ec.PageIDMap.RegisterNewPageID(uri, *op.PageID)
		}
	}
	operations.ResolvedLinks = l.resolveLinks(rawLinks, operations.UpsertOperations)
	return nil
}

// resolveLinks converts WikiLinks (page_ids) to StoredLinks (URIs). It
// mirrors ExtractLoop._resolve_links.
func (l *ExtractLoop) resolveLinks(rawLinks []map[string]any, upserts []ResolvedOperation) []StoredLink {
	if len(rawLinks) == 0 {
		return nil
	}
	ec := l.ContextProvider.GetExtractContext()
	pageIDMap := ec.PageIDMap
	opPageMap := make(map[int][]string)
	for _, op := range upserts {
		if op.PageID == nil || len(op.URIs) == 0 {
			continue
		}
		for _, uri := range op.URIs {
			opPageMap[*op.PageID] = append(opPageMap[*op.PageID], uri)
		}
	}
	if !pageIDMap.HasLinksEnabled() && len(opPageMap) == 0 {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	resolved := make([]StoredLink, 0, len(rawLinks))
	seen := make(map[string]struct{}, len(rawLinks))
	for _, raw := range rawLinks {
		fromPage, _ := asInt(raw["f"])
		toPage, _ := asInt(raw["t"])
		if fromPage == 0 || toPage == 0 {
			continue
		}
		fromURIs := make([]string, 0, 1)
		toURIs := make([]string, 0, 1)
		if uri := pageIDMap.Resolve(fromPage); uri != "" {
			fromURIs = append(fromURIs, uri)
		}
		if uri := pageIDMap.Resolve(toPage); uri != "" {
			toURIs = append(toURIs, uri)
		}
		for _, uri := range opPageMap[fromPage] {
			fromURIs = appendIfMissing(fromURIs, uri)
		}
		for _, uri := range opPageMap[toPage] {
			toURIs = appendIfMissing(toURIs, uri)
		}
		if len(fromURIs) == 0 || len(toURIs) == 0 {
			continue
		}
		linkType, _ := raw["link_type"].(string)
		if linkType == "" {
			linkType = LinkTypeDefault
		}
		weight := 0.5
		if w, ok := asFloat(raw["weight"]); ok {
			weight = w
		}
		description, _ := raw["description"].(string)
		var matchText *string
		if mt, ok := raw["match_text"].(string); ok && mt != "" {
			s := mt
			matchText = &s
		}
		for _, fromURI := range fromURIs {
			for _, toURI := range toURIs {
				if fromURI == toURI {
					continue
				}
				key := fromURI + "\x00" + toURI + "\x00" + linkType
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				resolved = append(resolved, StoredLink{
					FromURI:     fromURI,
					ToURI:       toURI,
					LinkType:    linkType,
					Weight:      weight,
					MatchText:   matchText,
					Description: description,
					CreatedAt:   now,
				})
			}
		}
	}
	return resolved
}

// checkUnreadExistingFiles returns URIs the LLM wants to write that
// exist on disk but were not read during prefetch. It mirrors
// ExtractLoop._check_unread_existing_files.
func (l *ExtractLoop) checkUnreadExistingFiles(ctx context.Context, operations *ResolvedOperations) map[string]any {
	if l.FS == nil {
		return nil
	}
	out := make(map[string]any, 4)
	for _, op := range operations.UpsertOperations {
		for _, uri := range op.URIs {
			if _, ok := l.ContextProvider.ReadFileContents()[uri]; ok {
				continue
			}
			content, err := l.FS.ReadFile(ctx, uri, l.Ctx)
			if err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					continue
				}
				continue
			}
			if content == "" {
				continue
			}
			mf := MemoryFileRead(content, uri)
			out[uri] = mf.ToMetadata()
		}
	}
	return out
}

// addRefetchResultsToMessages appends read-tool results for refetched
// URIs. It mirrors ExtractLoop._add_refetch_results_to_messages.
func (l *ExtractLoop) addRefetchResultsToMessages(messages []map[string]any, refetchURIs map[string]any) {
	callID := 1000
	for uri, parsed := range refetchURIs {
		messages = AddToolCallPairToMessages(messages, callID, "read", map[string]any{"uri": uri}, parsed)
		callID++
	}
	messages = append(messages, map[string]any{
		"role": "user",
		"content": "Note: The files above were automatically read because they exist and you didn't read them before deciding to write. Please consider the existing content when making write decisions. You can now output updated operations.",
	})
}

// addFormatErrorMessage appends a format-error guidance message. It
// mirrors ExtractLoop._add_format_error_message.
func (l *ExtractLoop) addFormatErrorMessage(messages []map[string]any) {
	messages = append(messages, map[string]any{
		"role": "user",
		"content": "Your previous output could not be parsed as valid JSON. " +
			"Please output ONLY a valid JSON object matching the required schema. " +
			"Do not include any explanation, markdown formatting, or text outside the JSON.",
	})
}

// buildFinalOperationsInstruction returns the instruction telling the
// LLM to produce final operations. It mirrors
// ExtractLoop._build_final_operations_instruction.
func (l *ExtractLoop) buildFinalOperationsInstruction() string {
	fields := []string{"delete_ids"}
	fields = append(fields, l.expectedFields...)
	skeleton := make(map[string][]any, len(fields))
	for _, f := range fields {
		skeleton[f] = []any{}
	}
	data, _ := json.Marshal(skeleton)
	return "You have reached the maximum number of tool call iterations. " +
		"Do not call any more tools. Return your final result now as ONLY a valid JSON object " +
		"matching the required schema. Do not include explanations or markdown. " +
		"If there are no memory changes, return this exact empty-shape JSON with all fields present:\n" +
		string(data)
}

// toItemList coerces v into a list of item maps.
func toItemList(v any) []map[string]any {
	switch x := v.(type) {
	case []map[string]any:
		return x
	case []any:
		out := make([]map[string]any, 0, len(x))
		for _, item := range x {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	case map[string]any:
		return []map[string]any{x}
	}
	return nil
}

// toDeleteIDs coerces v into a list of DeleteId.
func toDeleteIDs(v any) []DeleteId {
	items := toItemList(v)
	out := make([]DeleteId, 0, len(items))
	for _, item := range items {
		did := DeleteId{}
		if v, ok := item["delete_page_id"]; ok {
			if i, ok := asInt(v); ok {
				did.DeletePageID = &i
			}
		}
		if v, ok := item["replacement_page_id"]; ok {
			if v == nil {
				// nil means pure delete; leave ReplacementPageID nil.
				did.ReplacementPageID = nil
			} else if i, ok := asInt(v); ok {
				did.ReplacementPageID = &i
			}
		}
		out = append(out, did)
	}
	return out
}

// toRawLinks coerces v into a list of link maps.
func toRawLinks(v any) []map[string]any {
	return toItemList(v)
}

// appendIfMissing appends s to list when not already present.
func appendIfMissing(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}
