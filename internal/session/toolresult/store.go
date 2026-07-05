// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package toolresult

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/saker-ai/ctxhub/internal/domain"
	"github.com/saker-ai/ctxhub/internal/ragfs"
)

// StoredToolResult is the result of persisting one tool output. Mirrors
// openviking.session.tool_result_store.StoredToolResult.
type StoredToolResult struct {
	ToolResultID string
	StorageURI   string
	OutputURI    string
	MetadataURI  string
	Metadata     map[string]any
	Synopsis     ToolResultSynopsis
}

// Store persists raw tool outputs outside session messages so the bot
// can replace large outputs with a small synopsis stub. It is the Go
// counterpart of openviking.session.tool_result_store.ToolResultStore.
//
// The store is path-oriented: it writes one directory per tool result
// under {sessionURI}/tool-results/{toolResultID}/, containing output.txt
// (the raw bytes) and metadata.json (the synopsis + provenance).
type Store struct {
	fs         ragfs.FileSystem
	sessionURI string
	sessionID  string
}

// NewStore returns a Store backed by fs. sessionURI is the RAGFS prefix
// for the owning session (e.g. "sessions/{sessionID}").
func NewStore(fs ragfs.FileSystem, sessionURI, sessionID string) *Store {
	return &Store{fs: fs, sessionURI: sessionURI, sessionID: sessionID}
}

// WriteOptions are the inputs to Write. Pointer-typed fields are optional.
type WriteOptions struct {
	Content      string
	ToolID       string
	ToolName     string
	MessageID    string
	UserID       string
	PeerID       string
	CreatedAt    string
	PreviewChars int
	MimeType     string
	Synopsis     *ToolResultSynopsis // optional pre-computed synopsis
}

// Write persists content under a deterministic tool_result_id and returns
// the storage metadata. When the same content was already persisted for
// the same tool_id, the existing record is returned unchanged.
func (s *Store) Write(ctx context.Context, opts WriteOptions) (*StoredToolResult, error) {
	if s.fs == nil {
		return nil, fmt.Errorf("toolresult: filesystem is required")
	}
	if opts.Content == "" {
		return nil, fmt.Errorf("toolresult: content is empty")
	}
	digest := SHA256Text(opts.Content)
	toolResultID := BuildToolResultID(opts.ToolID, digest)
	storageURI := s.resultURI(toolResultID)
	outputURI := storageURI + "/output.txt"
	metadataURI := storageURI + "/metadata.json"

	if existing, err := s.ReadMetadata(ctx, toolResultID); err == nil {
		if existingDigest, _ := existing["sha256"].(string); existingDigest == digest {
			syn := SynopsisFromMap(asMap(existing["synopsis"]))
			return &StoredToolResult{
				ToolResultID: toolResultID,
				StorageURI:   storageURI,
				OutputURI:    outputURI,
				MetadataURI:  metadataURI,
				Metadata:     existing,
				Synopsis:     syn,
			}, nil
		}
	}

	syn := ToolResultSynopsis{}
	if opts.Synopsis != nil {
		syn = *opts.Synopsis
	} else {
		syn = GenerateSynopsis(opts.Content, opts.PreviewChars, opts.ToolName, opts.MimeType)
	}
	previewChars := minInt(len(opts.Content), maxInt(opts.PreviewChars, 0))
	metadata := map[string]any{
		"tool_result_id": toolResultID,
		"session_id":     s.sessionID,
		"message_id":     opts.MessageID,
		"tool_id":        opts.ToolID,
		"tool_name":      opts.ToolName,
		"user_id":        opts.UserID,
		"peer_id":        opts.PeerID,
		"created_at":     opts.CreatedAt,
		"original_chars": len(opts.Content),
		"preview_chars":  previewChars,
		"sha256":         digest,
		"mime_type":      orDefault(opts.MimeType, "text/plain"),
		"synopsis_kind":  string(syn.Kind),
		"synopsis":       syn.ToMap(),
		"storage_uri":    storageURI,
		"output_uri":     outputURI,
		"offset_unit":    "unicode_code_point",
	}

	if err := s.fs.Mkdir(ctx, storageURI, 0o755); err != nil && !isAlreadyExists(err) {
		return nil, fmt.Errorf("toolresult: create dir: %w", err)
	}
	if err := s.fs.Write(ctx, outputURI, strings.NewReader(opts.Content), 0o644); err != nil {
		return nil, fmt.Errorf("toolresult: write output: %w", err)
	}
	rawMeta, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("toolresult: marshal metadata: %w", err)
	}
	if err := s.fs.Write(ctx, metadataURI, bytes.NewReader(rawMeta), 0o644); err != nil {
		return nil, fmt.Errorf("toolresult: write metadata: %w", err)
	}

	return &StoredToolResult{
		ToolResultID: toolResultID,
		StorageURI:   storageURI,
		OutputURI:    outputURI,
		MetadataURI:  metadataURI,
		Metadata:     metadata,
		Synopsis:     syn,
	}, nil
}

// ReadMetadata reads and parses the metadata.json for toolResultID.
// Returns domain.ErrNotFound when the metadata file is missing.
func (s *Store) ReadMetadata(ctx context.Context, toolResultID string) (map[string]any, error) {
	if !ValidateToolResultID(toolResultID) {
		return nil, fmt.Errorf("%w: invalid tool_result_id", domain.ErrValidation)
	}
	metadataURI := s.resultURI(toolResultID) + "/metadata.json"
	var buf bytes.Buffer
	if err := s.fs.Read(ctx, metadataURI, &buf); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNotFound, err)
	}
	var out map[string]any
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("%w: invalid tool result metadata: %v", domain.ErrValidation, err)
	}
	return out, nil
}

// ReadOptions control Read behavior. Limit=-1 returns the entire content.
type ReadOptions struct {
	Offset           int
	Limit            int
	IncludeMetadata  bool
}

// ReadResult is the paged view of one persisted tool result.
type ReadResult struct {
	ToolResultID string         `json:"tool_result_id"`
	Content      string         `json:"content"`
	Offset       int            `json:"offset"`
	Limit        int            `json:"limit"`
	OffsetUnit   string         `json:"offset_unit"`
	TotalChars   int            `json:"total_chars"`
	HasMore      bool           `json:"has_more"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

// Read returns a slice of the persisted output starting at offset. Limit
// is the max chars to return; -1 means "the rest". IncludeMetadata
// attaches the metadata map to the result.
func (s *Store) Read(ctx context.Context, toolResultID string, opts ReadOptions) (*ReadResult, error) {
	if opts.Offset < 0 {
		return nil, fmt.Errorf("%w: offset must be >= 0", domain.ErrValidation)
	}
	if opts.Limit < -1 {
		return nil, fmt.Errorf("%w: limit must be -1 or >= 0", domain.ErrValidation)
	}
	metadata, err := s.ReadMetadata(ctx, toolResultID)
	if err != nil {
		return nil, err
	}
	outputURI := s.resultURI(toolResultID) + "/output.txt"
	var buf bytes.Buffer
	if err := s.fs.Read(ctx, outputURI, &buf); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNotFound, err)
	}
	content := buf.String()
	end := len(content)
	if opts.Limit != -1 {
		end = opts.Offset + opts.Limit
		if end > len(content) {
			end = len(content)
		}
	}
	if opts.Offset > len(content) {
		opts.Offset = len(content)
	}
	if end < opts.Offset {
		end = opts.Offset
	}
	chunk := content[opts.Offset:end]
	hasMore := opts.Limit != -1 && end < len(content)
	result := &ReadResult{
		ToolResultID: toolResultID,
		Content:      chunk,
		Offset:       opts.Offset,
		Limit:        opts.Limit,
		OffsetUnit:   "unicode_code_point",
		TotalChars:   len(content),
		HasMore:      hasMore,
	}
	if opts.IncludeMetadata {
		result.Metadata = metadata
	}
	return result, nil
}

// SearchOptions bound a substring search over one tool result.
type SearchOptions struct {
	Query       string
	Limit       int
	ContextChars int
}

// SearchMatch is one occurrence of the query with surrounding context.
type SearchMatch struct {
	Offset     int    `json:"offset"`
	OffsetUnit string `json:"offset_unit"`
	Snippet    string `json:"snippet"`
}

// SearchResult is the list of matches for one tool result.
type SearchResult struct {
	ToolResultID string         `json:"tool_result_id"`
	Matches      []SearchMatch  `json:"matches"`
}

// Search finds up to opts.Limit occurrences of opts.Query in the persisted
// output, returning a snippet of ContextChars around each hit.
func (s *Store) Search(ctx context.Context, toolResultID string, opts SearchOptions) (*SearchResult, error) {
	if opts.Query == "" {
		return nil, fmt.Errorf("%w: query must not be empty", domain.ErrValidation)
	}
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be > 0", domain.ErrValidation)
	}
	outputURI := s.resultURI(toolResultID) + "/output.txt"
	var buf bytes.Buffer
	if err := s.fs.Read(ctx, outputURI, &buf); err != nil {
		return nil, fmt.Errorf("%w: %v", domain.ErrNotFound, err)
	}
	content := buf.String()
	matches := []SearchMatch{}
	start := 0
	for len(matches) < opts.Limit {
		idx := strings.Index(content[start:], opts.Query)
		if idx < 0 {
			break
		}
		idx += start
		left := idx - opts.ContextChars
		if left < 0 {
			left = 0
		}
		right := idx + len(opts.Query) + opts.ContextChars
		if right > len(content) {
			right = len(content)
		}
		matches = append(matches, SearchMatch{
			Offset:     idx,
			OffsetUnit: "unicode_code_point",
			Snippet:    content[left:right],
		})
		start = idx + len(opts.Query)
		if start >= len(content) {
			break
		}
	}
	return &SearchResult{ToolResultID: toolResultID, Matches: matches}, nil
}

// ListOptions bounds a listing over the session's tool results.
type ListOptions struct {
	ToolName string
	Limit    int
}

// ListResult is the listing of tool results under the session.
type ListResult struct {
	ToolResults []map[string]any `json:"tool_results"`
}

// List enumerates persisted tool results. When ToolName is set, only
// results whose metadata.tool_name matches are returned.
func (s *Store) List(ctx context.Context, opts ListOptions) (*ListResult, error) {
	if opts.Limit <= 0 {
		return nil, fmt.Errorf("%w: limit must be > 0", domain.ErrValidation)
	}
	entries, err := s.fs.ReadDir(ctx, s.baseURI())
	if err != nil {
		if isNotFound(err) {
			return &ListResult{ToolResults: []map[string]any{}}, nil
		}
		return nil, fmt.Errorf("toolresult: list: %w", err)
	}
	results := []map[string]any{}
	for _, entry := range entries {
		if entry.Info == nil || !entry.Info.IsDir {
			continue
		}
		name := entry.Info.Name
		if name == "" {
			name = entry.RelPath
		}
		meta, err := s.ReadMetadata(ctx, name)
		if err != nil {
			continue
		}
		if opts.ToolName != "" {
			if n, _ := meta["tool_name"].(string); n != opts.ToolName {
				continue
			}
		}
		results = append(results, meta)
		if len(results) >= opts.Limit {
			break
		}
	}
	return &ListResult{ToolResults: results}, nil
}

// --- helpers ---

func (s *Store) baseURI() string  { return s.sessionURI + "/tool-results" }
func (s *Store) resultURI(id string) string { return s.baseURI() + "/" + id }

// SHA256Text returns the hex-encoded SHA-256 of content (UTF-8).
func SHA256Text(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// MakePreview builds the deterministic stub that replaces the raw output
// in session messages. It is the Go counterpart of
// openviking.session.tool_result_store.make_preview.
func MakePreview(content string, previewChars int, ref, toolName, sha256, reason, mimeType string) string {
	original := len(content)
	syn := GenerateSynopsis(content, previewChars, toolName, mimeType)
	return RenderToolResultStub(syn, ref, toolName, sha256, reason, original, minInt(len(content), maxInt(previewChars, 0)))
}

func asMap(v any) map[string]any {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	return map[string]any{}
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ragfs.ErrAlreadyExists) {
		return true
	}
	// Backends may return os.ErrExist or domain.ErrConflict.
	return errors.Is(err, domain.ErrConflict)
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, domain.ErrNotFound)
}

// io.Reader adapter so we can pass strings.NewReader / bytes.NewReader
// without forcing callers to import a particular type.
var _ io.Reader = (*strings.Reader)(nil)
