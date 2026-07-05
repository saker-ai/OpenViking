// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// LLMHiddenMemoryFields is the set of metadata fields stripped from
// the LLM-visible read output. It mirrors
// openviking.session.memory.tools._LLM_HIDDEN_MEMORY_FIELDS.
var LLMHiddenMemoryFields = map[string]struct{}{
	"source_extraction_id":  {},
	"source_extraction_ids": {},
	"last_update_trace_id":  {},
}

// ToolContext is the minimal context passed to memory tools. It
// mirrors the subset of openviking.server.identity.ToolContext that
// the memory tools actually use.
//
// The fields are intentionally interface-typed because the concrete
// VikingFS, RequestContext, and supporting types are not yet in the
// Go tree. Once they land, narrow them to typed pointers.
type ToolContext struct {
	// VikingFS is the filesystem used by read/search/ls tools.
	VikingFS VikingFS
	// RequestContext carries request-scoped auth/user info. Passed
	// through to VikingFS calls.
	RequestContext any
	// ReadFileContents caches parsed MemoryFile by URI so the extract
	// loop can reuse the read result without re-parsing.
	ReadFileContents map[string]MemoryFile
	// PageIDMap maps URIs to page_ids for link extraction.
	PageIDMap *PageIdMap
	// DefaultSearchURIs is the search scope (directory URI or empty).
	DefaultSearchURIs string
}

// VikingFS is the minimal filesystem interface used by memory tools.
// It mirrors the subset of openviking.storage.viking_fs.VikingFS that
// the memory tools actually call.
type VikingFS interface {
	// ReadFile reads the content at uri.
	ReadFile(ctx context.Context, uri string, requestCtx any) (string, error)
	// Search performs a semantic search.
	Search(ctx context.Context, query, targetURI string, limit int, requestCtx any) (SearchResult, error)
	// Ls lists directory contents.
	Ls(ctx context.Context, uri, output string, absLimit int, showAllHidden bool, nodeLimit int, requestCtx any) ([]LsEntry, error)
}

// SearchResult is the search result returned by VikingFS.Search. It
// mirrors the subset of openviking.storage.viking_fs.SearchResult used
// by the memory tools.
type SearchResult interface {
	// ToDict returns the search result as a map[string]any with
	// "memories" (a list of {uri, score, ...}) and optional "error".
	ToDict() map[string]any
}

// LsEntry is one directory entry returned by VikingFS.Ls. It mirrors
// the subset of openviking.storage.viking_fs.LsEntry used by the
// memory tools.
type LsEntry map[string]any

// MemoryTool is the abstract base class for memory tools. It mirrors
// openviking.session.memory.tools.MemoryTool.
//
// The Python original is async; the Go counterpart takes a context
// and returns (any, error).
type MemoryTool interface {
	// Name is the tool name used in function calls.
	Name() string
	// Description describes what the tool does.
	Description() string
	// Parameters returns the JSON-schema for tool parameters.
	Parameters() map[string]any
	// Execute runs the tool with the given parameters.
	Execute(ctx context.Context, tc *ToolContext, kwargs map[string]any) (any, error)
	// ToSchema returns the OpenAI function schema format.
	ToSchema() map[string]any
}

// BaseMemoryTool provides a default ToSchema implementation. Embed it
// in concrete tool structs to satisfy the MemoryTool interface without
// duplicating the schema boilerplate.
type BaseMemoryTool struct{}

// ToSchema returns the OpenAI function schema format. It mirrors
// MemoryTool.to_schema.
func (BaseMemoryTool) ToSchema(tool MemoryTool) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
			"parameters":  tool.Parameters(),
		},
	}
}

// MemoryReadTool reads a single memory file. It mirrors
// openviking.session.memory.tools.MemoryReadTool.
type MemoryReadTool struct {
	BaseMemoryTool
}

// NewMemoryReadTool returns a MemoryReadTool.
func NewMemoryReadTool() *MemoryReadTool { return &MemoryReadTool{} }

// Name returns "read".
func (t *MemoryReadTool) Name() string { return "read" }

// Description returns the tool description.
func (t *MemoryReadTool) Description() string { return "Read single file" }

// Parameters returns the JSON-schema for the read tool parameters.
func (t *MemoryReadTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"uri": map[string]any{
				"type":        "string",
				"description": "Memory URI to read, e.g., 'viking://user/user123/memories/profile.md'",
			},
			"offset": map[string]any{
				"type":        "integer",
				"description": "Starting line number to read from (0-indexed)",
				"default":     0,
				"minimum":     0,
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Number of lines to read. -1 means read to end",
				"default":     -1,
				"minimum":     -1,
			},
		},
		"required": []string{"uri"},
	}
}

// Execute reads a memory file and returns a metadata dict. It mirrors
// MemoryReadTool.execute.
//
// The returned dict includes:
//   - parsed MEMORY_FIELDS metadata (with links/backlinks/hidden fields stripped)
//   - page_id when ctx.PageIDMap is configured
//   - line-numbered content sliced by offset/limit
//
// On file-not-found or read errors, returns {"error": <summary>}.
func (t *MemoryReadTool) Execute(ctx context.Context, tc *ToolContext, kwargs map[string]any) (any, error) {
	uri, _ := kwargs["uri"].(string)
	offset := 0
	if v, ok := asInt(kwargs["offset"]); ok {
		offset = v
	}
	limit := -1
	if v, ok := asInt(kwargs["limit"]); ok {
		limit = v
	}
	if tc == nil || tc.VikingFS == nil {
		return map[string]any{"error": "ToolContext not configured"}, nil
	}
	content, err := tc.VikingFS.ReadFile(ctx, uri, tc.RequestContext)
	if err != nil {
		return map[string]any{"error": ExtractErrorSummary(err.Error())}, nil
	}
	mf := MemoryFileRead(content, uri)
	if tc.ReadFileContents != nil {
		tc.ReadFileContents[uri] = mf
	}
	llmResult := mf.ToMetadata()
	delete(llmResult, "links")
	delete(llmResult, "backlinks")
	for hidden := range LLMHiddenMemoryFields {
		delete(llmResult, hidden)
	}
	if tc.PageIDMap != nil {
		pageID := tc.PageIDMap.GetPageID(uri)
		llmResult["page_id"] = pageID
	}
	plainContent := mf.PlainContent()
	visibleContent := SliceContentLines(plainContent, offset, limit)
	switch {
	case visibleContent != "":
		llmResult["content"] = AddLineNumbers(visibleContent, offset+1)
	case LineCount(plainContent) == 0:
		llmResult["content"] = "<system-reminder>Warning: the file exists but the contents are empty.</system-reminder>"
	default:
		llmResult["content"] = fmt.Sprintf(
			"<system-reminder>Warning: the file exists but is shorter than the provided offset (%d). The file has %d lines.</system-reminder>",
			offset+1, LineCount(plainContent),
		)
	}
	return llmResult, nil
}

// ToSchema returns the OpenAI function schema format for the read tool.
func (t *MemoryReadTool) ToSchema() map[string]any { return t.BaseMemoryTool.ToSchema(t) }

// MemorySearchTool performs a semantic search. It mirrors
// openviking.session.memory.tools.MemorySearchTool.
type MemorySearchTool struct {
	BaseMemoryTool
}

// NewMemorySearchTool returns a MemorySearchTool.
func NewMemorySearchTool() *MemorySearchTool { return &MemorySearchTool{} }

// Name returns "search".
func (t *MemorySearchTool) Name() string { return "search" }

// Description returns the tool description.
func (t *MemorySearchTool) Description() string { return "Semantic search with session context" }

// Parameters returns the JSON-schema for the search tool parameters.
func (t *MemorySearchTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"query": map[string]any{
				"type":        "string",
				"description": "Search query text",
			},
			"limit": map[string]any{
				"type":        "integer",
				"description": "Maximum results to return, default 10",
				"default":     10,
			},
		},
		"required": []string{"query"},
	}
}

// Execute performs a semantic search and returns the optimized result.
// It mirrors MemorySearchTool.execute.
func (t *MemorySearchTool) Execute(ctx context.Context, tc *ToolContext, kwargs map[string]any) (any, error) {
	query, _ := kwargs["query"].(string)
	limit := 10
	if v, ok := asInt(kwargs["limit"]); ok && v > 0 {
		limit = v
	}
	if tc == nil || tc.VikingFS == nil {
		return map[string]any{"error": "ToolContext not configured"}, nil
	}
	targetURI := tc.DefaultSearchURIs
	result, err := tc.VikingFS.Search(ctx, query, targetURI, limit+10, tc.RequestContext)
	if err != nil {
		return map[string]any{"error": ExtractErrorSummary(err.Error())}, nil
	}
	var resultDict map[string]any
	if result != nil {
		resultDict = result.ToDict()
	}
	return OptimizeSearchResult(resultDict, limit), nil
}

// ToSchema returns the OpenAI function schema format for the search tool.
func (t *MemorySearchTool) ToSchema() map[string]any { return t.BaseMemoryTool.ToSchema(t) }

// MemoryLsTool lists directory contents. It mirrors
// openviking.session.memory.tools.MemoryLsTool.
type MemoryLsTool struct {
	BaseMemoryTool
}

// NewMemoryLsTool returns a MemoryLsTool.
func NewMemoryLsTool() *MemoryLsTool { return &MemoryLsTool{} }

// Name returns "ls".
func (t *MemoryLsTool) Name() string { return "ls" }

// Description returns the tool description.
func (t *MemoryLsTool) Description() string {
	return "List directory content, includes abstract field when output='agent'"
}

// Parameters returns the JSON-schema for the ls tool parameters.
func (t *MemoryLsTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"uri": map[string]any{
				"type":        "string",
				"description": "Directory URI to list, e.g., 'viking://user/user123/memories'",
			},
		},
		"required": []string{"uri"},
	}
}

// Execute lists a directory and returns a formatted string. It mirrors
// MemoryLsTool.execute.
func (t *MemoryLsTool) Execute(ctx context.Context, tc *ToolContext, kwargs map[string]any) (any, error) {
	uri, _ := kwargs["uri"].(string)
	if tc == nil || tc.VikingFS == nil {
		return map[string]any{"error": "ToolContext not configured"}, nil
	}
	entries, err := tc.VikingFS.Ls(ctx, uri, "agent", 256, false, 1000, tc.RequestContext)
	if err != nil {
		return map[string]any{"error": ExtractErrorSummary(err.Error())}, nil
	}
	var lines []string
	for _, e := range entries {
		if isDir, _ := e["isDir"].(bool); isDir {
			continue
		}
		name, _ := e["name"].(string)
		if name == "" {
			if u, ok := e["uri"].(string); ok {
				if idx := strings.LastIndex(u, "/"); idx >= 0 {
					name = u[idx+1:]
				} else {
					name = u
				}
			}
		}
		size := int64(0)
		if s, ok := asInt(e["size"]); ok {
			size = int64(s)
		}
		lines = append(lines, fmt.Sprintf("%s %s", name, FormatSize(size)))
	}
	if len(lines) == 0 {
		return "Directory is empty. You can write new files to create memory content.", nil
	}
	return strings.Join(lines, "\n"), nil
}

// ToSchema returns the OpenAI function schema format for the ls tool.
func (t *MemoryLsTool) ToSchema() map[string]any { return t.BaseMemoryTool.ToSchema(t) }

// MemoryToolsRegistry is the registry for memory tools. It mirrors the
// module-level MEMORY_TOOLS_REGISTRY in tools.py.
//
// The Python original is a module-level dict. The Go counterpart is a
// struct with a mutex so callers can safely register tools from
// multiple goroutines.
type MemoryToolsRegistry struct {
	mu    sync.RWMutex
	tools map[string]MemoryTool
}

// NewMemoryToolsRegistry returns an empty registry.
func NewMemoryToolsRegistry() *MemoryToolsRegistry {
	return &MemoryToolsRegistry{tools: make(map[string]MemoryTool)}
}

// Register registers a memory tool. Subsequent registrations with the
// same name overwrite the previous one. It mirrors register_tool.
func (r *MemoryToolsRegistry) Register(tool MemoryTool) {
	if r == nil || tool == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

// Get returns the tool with the given name, or nil when absent. It
// mirrors get_tool.
func (r *MemoryToolsRegistry) Get(name string) MemoryTool {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.tools[name]
}

// LLMTools is the set of tool names exposed to the LLM. It mirrors the
// module-level LLM_TOOLS list in tools.py.
var LLMTools = map[string]struct{}{
	"read": {},
}

// GetToolSchemas returns the schemas for tools exposed to the LLM, in
// OpenAI function schema format. It mirrors get_tool_schemas.
func (r *MemoryToolsRegistry) GetToolSchemas() []map[string]any {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]map[string]any, 0, len(r.tools))
	for name, tool := range r.tools {
		if _, ok := LLMTools[name]; !ok {
			continue
		}
		out = append(out, tool.ToSchema())
	}
	return out
}

// DefaultMemoryTools is the process-wide registry populated with the
// default read/search/ls tools. It mirrors the module-level
// MEMORY_TOOLS_REGISTRY after register_tool has been called for the
// three defaults.
//
// Callers that want isolation (e.g. tests) should construct their own
// MemoryToolsRegistry via NewMemoryToolsRegistry instead of using this
// global.
var DefaultMemoryTools = func() *MemoryToolsRegistry {
	r := NewMemoryToolsRegistry()
	r.Register(NewMemoryReadTool())
	r.Register(NewMemorySearchTool())
	r.Register(NewMemoryLsTool())
	return r
}()

// RegisterTool registers a tool on the default registry. It mirrors the
// module-level register_tool function.
//
// Deprecated: prefer NewMemoryToolsRegistry + Register for testability.
func RegisterTool(tool MemoryTool) { DefaultMemoryTools.Register(tool) }

// GetTool returns the tool with the given name from the default
// registry, or nil when absent. It mirrors the module-level get_tool
// function.
//
// Deprecated: prefer MemoryToolsRegistry.Get for testability.
func GetTool(name string) MemoryTool { return DefaultMemoryTools.Get(name) }

// GetToolSchemas returns the schemas for tools exposed to the LLM from
// the default registry. It mirrors the module-level get_tool_schemas
// function.
//
// Deprecated: prefer MemoryToolsRegistry.GetToolSchemas for testability.
func GetToolSchemas() []map[string]any { return DefaultMemoryTools.GetToolSchemas() }
