// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// MemoryFS is the storage interface used by the memory updater. It
// extends the read-only VikingFS interface with the write/delete methods
// the updater needs. Concrete VikingFS implementations in the storage
// layer satisfy this interface; tests provide in-memory stubs.
type MemoryFS interface {
	VikingFS
	// WriteFile writes content to uri atomically.
	WriteFile(ctx context.Context, uri, content string, requestCtx any) error
	// Rm removes uri. When recursive is true, directory contents are
	// removed as well. Missing files are not an error.
	Rm(ctx context.Context, uri string, recursive bool, requestCtx any) error
}

// ChunkMeta is metadata for a derived extraction chunk message. It
// mirrors openviking.session.memory.memory_updater.ChunkMeta.
type ChunkMeta struct {
	SourceMessageID string
	ChunkIndex      int
	ChunkCount      int
}

// ExtractContext carries the messages, page_id_map, and chunk metadata
// used by template rendering during memory extraction. It mirrors
// openviking.session.memory.memory_updater.ExtractContext.
//
// The Python original splits long text-only messages into derived
// chunks so event `ranges` can point to a narrower source span. The Go
// counterpart preserves the splitting behavior so callers that build
// ranges against the extraction messages see the same indices.
type ExtractContext struct {
	Messages   []Message
	ChunkMeta  map[string]ChunkMeta
	PageIDMap  *PageIdMap
	split      bool
}

// NewExtractContext constructs an ExtractContext. When
// splitLongTextMessages is true, long text-only messages are split into
// derived chunks (preserving the Python behavior used by event ranges).
func NewExtractContext(messages []Message, splitLongTextMessages bool) *ExtractContext {
	ec := &ExtractContext{
		PageIDMap: NewPageIdMap(),
		ChunkMeta: make(map[string]ChunkMeta),
	}
	if splitLongTextMessages {
		ec.Messages, ec.ChunkMeta = ec.buildExtractionMessages(messages)
		ec.split = true
	} else {
		ec.Messages = make([]Message, len(messages))
		copy(ec.Messages, messages)
	}
	return ec
}

// buildExtractionMessages splits long text-only messages into derived
// chunks. It mirrors ExtractContext._build_extraction_messages.
func (ec *ExtractContext) buildExtractionMessages(messages []Message) ([]Message, map[string]ChunkMeta) {
	out := make([]Message, 0, len(messages))
	chunkMeta := make(map[string]ChunkMeta, len(messages))
	for _, msg := range messages {
		parts := msg.Parts()
		allText := true
		text := strings.Builder{}
		for _, p := range parts {
			if p.IsImage() {
				allText = false
				break
			}
			text.WriteString(p.Text())
		}
		if !allText || text.Len() == 0 {
			out = append(out, msg)
			continue
		}
		chunks := splitTextForExtraction(text.String())
		if len(chunks) <= 1 {
			out = append(out, msg)
			continue
		}
		for idx, chunk := range chunks {
			chunkMsg := NewTextMessage(
				fmt.Sprintf("%s#chunk_%d", msg.ID(), idx),
				msg.Role(),
				msg.PeerID(),
				msg.CreatedAt(),
				[]MessagePart{NewTextOnlyPart(chunk)},
			)
			out = append(out, chunkMsg)
			chunkMeta[chunkMsg.ID()] = ChunkMeta{
				SourceMessageID: msg.ID(),
				ChunkIndex:      idx,
				ChunkCount:      len(chunks),
			}
		}
	}
	return out, chunkMeta
}

// ReadMessageRanges parses a ranges string like "0-10,50-60" or
// "7,9,11,13" and returns the combined MessageRange. It mirrors
// ExtractContext.read_message_ranges.
func (ec *ExtractContext) ReadMessageRanges(rangesStr string) *MessageRange {
	if ec == nil || rangesStr == "" {
		return &MessageRange{Elements: nil}
	}
	type rng struct{ start, end int }
	ranges := make([]rng, 0, 4)
	for _, part := range strings.Split(rangesStr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, "-"); idx >= 0 {
			startStr := strings.TrimSpace(part[:idx])
			endStr := strings.TrimSpace(part[idx+1:])
			s, err1 := atoiSafe(startStr)
			e, err2 := atoiSafe(endStr)
			if err1 != nil || err2 != nil {
				continue
			}
			ranges = append(ranges, rng{s, e})
		} else {
			n, err := atoiSafe(part)
			if err != nil {
				continue
			}
			ranges = append(ranges, rng{n, n})
		}
	}
	if len(ranges) == 0 {
		return &MessageRange{Elements: nil}
	}
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].start < ranges[j].start })
	merged := []rng{ranges[0]}
	for _, r := range ranges[1:] {
		prev := &merged[len(merged)-1]
		if r.start <= prev.end+1 {
			if r.end > prev.end {
				prev.end = r.end
			}
		} else {
			merged = append(merged, r)
		}
	}
	elements := make([][]Message, 0, len(merged))
	for _, r := range merged {
		start := r.start
		end := r.end
		if start < 0 {
			start = 0
		}
		if end >= len(ec.Messages) {
			end = len(ec.Messages) - 1
		}
		if start > end {
			continue
		}
		elements = append(elements, ec.Messages[start:end+1])
	}
	return &MessageRange{Elements: elements, ChunkMeta: ec.ChunkMeta}
}

// GetSessionTimestamp returns the YYYYMMDDHHMMSS timestamp of the first
// message with a parseable created_at, falling back to time.Now(). It
// mirrors ExtractContext.get_session_timestamp.
func (ec *ExtractContext) GetSessionTimestamp() string {
	for _, msg := range ec.Messages {
		if msg.CreatedAt() != "" {
			if ts := parseCompactTimestamp(msg.CreatedAt()); ts != "" {
				return ts
			}
		}
	}
	return time.Now().Format("20060102150405")
}

// GetTimestampFromRanges returns the compact timestamp for the first
// message in the range, falling back to time.Now(). It mirrors
// ExtractContext.get_timestamp_from_ranges.
func (ec *ExtractContext) GetTimestampFromRanges(rangesStr string) string {
	if rangesStr != "" {
		mr := ec.ReadMessageRanges(rangesStr)
		for _, group := range mr.Elements {
			for _, msg := range group {
				if msg.CreatedAt() != "" {
					if ts := parseCompactTimestamp(msg.CreatedAt()); ts != "" {
						return ts
					}
				}
			}
		}
	}
	return time.Now().Format("20060102150405")
}

// MessageRange represents a range of messages for formatting. It
// mirrors openviking.session.memory.memory_updater.MessageRange.
type MessageRange struct {
	Elements  [][]Message
	ChunkMeta map[string]ChunkMeta
}

// PrettyPrint prints the message range with "..." between non-contiguous
// groups. It mirrors MessageRange.pretty_print.
func (mr *MessageRange) PrettyPrint() string {
	if mr == nil || len(mr.Elements) == 0 {
		return ""
	}
	out := strings.Builder{}
	for i, group := range mr.Elements {
		for _, line := range mr.formatContiguousGroup(group) {
			out.WriteString(line)
			out.WriteString("\n")
		}
		if i < len(mr.Elements)-1 {
			out.WriteString("...\n")
		}
	}
	return out.String()
}

// formatContiguousGroup formats one contiguous message group. It mirrors
// MessageRange._format_contiguous_group.
func (mr *MessageRange) formatContiguousGroup(group []Message) []string {
	formatted := make([]string, 0, len(group))
	current := make([]Message, 0, len(group))
	flush := func() {
		if len(current) == 0 {
			return
		}
		content := mr.formatMergedContent(current)
		speaker := mr.speakerFor(current[0])
		formatted = append(formatted, fmt.Sprintf("[%s]: %s", speaker, content))
		current = current[:0]
	}
	for _, msg := range group {
		if len(current) > 0 && !mr.canMergeMessages(current[len(current)-1], msg) {
			flush()
		}
		current = append(current, msg)
	}
	flush()
	return formatted
}

// speakerFor returns the message's peer_id when set, otherwise its role.
func (mr *MessageRange) speakerFor(message Message) string {
	if message.PeerID() != "" {
		return message.PeerID()
	}
	return message.Role()
}

// canMergeMessages reports whether two messages can be merged into one
// formatted line. They can when they share chunk metadata from the same
// source message and consecutive chunk indices. It mirrors
// MessageRange._can_merge_messages.
func (mr *MessageRange) canMergeMessages(previous, current Message) bool {
	prevMeta := mr.chunkMetaFor(previous)
	curMeta := mr.chunkMetaFor(current)
	if prevMeta == nil || curMeta == nil {
		return false
	}
	if mr.speakerFor(previous) != mr.speakerFor(current) {
		return false
	}
	return prevMeta.SourceMessageID == curMeta.SourceMessageID &&
		curMeta.ChunkIndex == prevMeta.ChunkIndex+1
}

// formatMergedContent concatenates the text content of messages. It
// mirrors MessageRange._format_merged_content.
func (mr *MessageRange) formatMergedContent(messages []Message) string {
	out := strings.Builder{}
	for _, msg := range messages {
		for _, p := range msg.Parts() {
			out.WriteString(p.Text())
		}
	}
	return out.String()
}

// chunkMetaFor returns the chunk metadata for message, or nil when none.
func (mr *MessageRange) chunkMetaFor(message Message) *ChunkMeta {
	if mr == nil || mr.ChunkMeta == nil {
		return nil
	}
	if meta, ok := mr.ChunkMeta[message.ID()]; ok {
		return &meta
	}
	return nil
}

// FirstMessageTime returns the YYYY-MM-DD date of the first message with
// a parseable created_at. It mirrors MessageRange._first_message_time.
func (mr *MessageRange) FirstMessageTime() string {
	if mr == nil {
		return ""
	}
	for _, group := range mr.Elements {
		for _, msg := range group {
			if msg.CreatedAt() != "" {
				if t, err := time.Parse(time.RFC3339, msg.CreatedAt()); err == nil {
					return t.Format("2006-01-02")
				}
				if t, err := time.Parse("2006-01-02 15:04:05", msg.CreatedAt()); err == nil {
					return t.Format("2006-01-02")
				}
			}
		}
	}
	return ""
}

// FirstMessageTimeWithWeekday returns the first message date with the
// weekday name. It mirrors MessageRange._first_message_time_with_weekday.
func (mr *MessageRange) FirstMessageTimeWithWeekday() string {
	if mr == nil {
		return ""
	}
	weekdays := []string{"Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday"}
	for _, group := range mr.Elements {
		for _, msg := range group {
			if msg.CreatedAt() != "" {
				var t time.Time
				var err error
				if t, err = time.Parse(time.RFC3339, msg.CreatedAt()); err != nil {
					if t, err = time.Parse("2006-01-02 15:04:05", msg.CreatedAt()); err != nil {
						continue
					}
				}
				return fmt.Sprintf("%s (%s)", t.Format("2006-01-02"), weekdays[int(t.Weekday())])
			}
		}
	}
	return ""
}

// MemoryUpdateResult records the URIs written, edited, and deleted by
// one apply_operations call. It mirrors
// openviking.session.memory.memory_updater.MemoryUpdateResult.
type MemoryUpdateResult struct {
	WrittenURIs []string
	EditedURIs  []string
	DeletedURIs []string
	Errors      []URLError
}

// URLError is one URI/error pair recorded in MemoryUpdateResult.
type URLError struct {
	URI   string
	Error error
}

// AddWritten records a written URI.
func (r *MemoryUpdateResult) AddWritten(uri string) {
	if r == nil {
		return
	}
	r.WrittenURIs = append(r.WrittenURIs, uri)
}

// AddEdited records an edited URI.
func (r *MemoryUpdateResult) AddEdited(uri string) {
	if r == nil {
		return
	}
	r.EditedURIs = append(r.EditedURIs, uri)
}

// AddDeleted records a deleted URI.
func (r *MemoryUpdateResult) AddDeleted(uri string) {
	if r == nil {
		return
	}
	r.DeletedURIs = append(r.DeletedURIs, uri)
}

// AddError records a URI/error pair.
func (r *MemoryUpdateResult) AddError(uri string, err error) {
	if r == nil {
		return
	}
	r.Errors = append(r.Errors, URLError{URI: uri, Error: err})
}

// HasChanges reports whether the result contains any write/edit/delete.
func (r *MemoryUpdateResult) HasChanges() bool {
	if r == nil {
		return false
	}
	return len(r.WrittenURIs) > 0 || len(r.EditedURIs) > 0 || len(r.DeletedURIs) > 0
}

// Summary returns a one-line summary of the result counts.
func (r *MemoryUpdateResult) Summary() string {
	if r == nil {
		return "Written: 0, Edited: 0, Deleted: 0, Errors: 0"
	}
	return fmt.Sprintf("Written: %d, Edited: %d, Deleted: %d, Errors: %d",
		len(r.WrittenURIs), len(r.EditedURIs), len(r.DeletedURIs), len(r.Errors))
}

// MemoryUpdater is the interface that applies ResolvedOperations to
// storage. It mirrors the apply_operations surface of
// openviking.session.memory.memory_updater.MemoryUpdater.
//
// Both the concrete memoryUpdater (below) and StreamingMemoryUpdater
// implement this interface so callers can swap the synchronous updater
// for the batching streaming updater without changing call sites.
type MemoryUpdater interface {
	// ApplyOperations applies the resolved operations to storage. The
	// operations argument carries upsert_operations,
	// delete_file_contents, resolved_links, and delete_replacements.
	// Returns a MemoryUpdateResult summarizing the writes/edits/deletes.
	ApplyOperations(
		ctx context.Context,
		operations ResolvedOperations,
		requestCtx any,
		ec *ExtractContext,
		ih *MemoryIsolationHandler,
	) (*MemoryUpdateResult, error)
}

// memoryUpdater is the default synchronous MemoryUpdater. It mirrors
// openviking.session.memory.memory_updater.MemoryUpdater.
//
// The Python original vectorizes memories via vikingdb and refreshes
// overview files; the Go counterpart persists the write/edit/delete
// operations through MemoryFS and leaves vectorization to the caller
// (the vikingdb integration is not yet in the Go tree).
type memoryUpdater struct {
	fs           MemoryFS
	registry     *MemoryTypeRegistry
	mu           sync.Mutex
}

// Compile-time assertion that memoryUpdater implements MemoryUpdater.
var _ MemoryUpdater = (*memoryUpdater)(nil)

// NewMemoryUpdater returns a MemoryUpdater. The registry must be set
// (via NewMemoryUpdaterWithRegistry or SetRegistry) before
// ApplyOperations is called. The concrete *memoryUpdater is returned so
// callers can configure it; it satisfies the MemoryUpdater interface.
func NewMemoryUpdater() *memoryUpdater {
	return &memoryUpdater{}
}

// NewMemoryUpdaterWithRegistry returns a MemoryUpdater with the given
// registry and filesystem. The concrete *memoryUpdater is returned so
// callers can configure it; it satisfies the MemoryUpdater interface.
func NewMemoryUpdaterWithRegistry(registry *MemoryTypeRegistry, fs MemoryFS) *memoryUpdater {
	return &memoryUpdater{registry: registry, fs: fs}
}

// SetRegistry sets the memory type registry used for URI resolution.
func (u *memoryUpdater) SetRegistry(r *MemoryTypeRegistry) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.registry = r
}

// SetVikingFS sets the filesystem used for write/edit/delete.
func (u *memoryUpdater) SetVikingFS(fs MemoryFS) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.fs = fs
}

// MemoryTypeFromURI extracts the memory_type segment from a Viking URI.
// Returns "" when the URI does not contain a "memories" segment. It
// mirrors MemoryUpdater.memory_type_from_uri.
func MemoryTypeFromURI(uri string) string {
	parts := strings.Split(uri, "/")
	idx := -1
	for i, p := range parts {
		if p == "memories" {
			idx = i
			break
		}
	}
	if idx < 0 || idx+1 >= len(parts) {
		return ""
	}
	return parts[idx+1]
}

// ApplyOperations applies the resolved operations to storage. It mirrors
// MemoryUpdater.apply_operations.
//
// The operations argument carries upsert_operations, delete_file_contents,
// resolved_links, and delete_replacements. The handler resolves URIs,
// applies merge_ops field-by-field, writes the new content, and then
// applies delete + link-backfill passes.
func (u *memoryUpdater) ApplyOperations(
	ctx context.Context,
	operations ResolvedOperations,
	requestCtx any,
	ec *ExtractContext,
	ih *MemoryIsolationHandler,
) (*MemoryUpdateResult, error) {
	result := &MemoryUpdateResult{}
	u.mu.Lock()
	fs := u.fs
	registry := u.registry
	u.mu.Unlock()
	if fs == nil {
		return nil, errors.New("MemoryFS not configured")
	}
	if registry == nil {
		return nil, errors.New("MemoryTypeRegistry is required for URI resolution")
	}
	if operations.HasErrors() {
		for _, e := range operations.Errors {
			result.AddError("unknown", errors.New(e))
		}
		return result, nil
	}
	// Reject upserts with no resolved URIs.
	for _, op := range operations.UpsertOperations {
		if len(op.URIs) == 0 {
			return nil, fmt.Errorf("Cannot apply operations: missing resolved URIs for %s(page_id=%v)", op.MemoryType, op.PageID)
		}
	}
	// Apply upserts.
	for _, op := range operations.UpsertOperations {
		if err := u.applyUpsert(ctx, op, requestCtx, ec); err != nil {
			for _, uri := range op.URIs {
				result.AddError(uri, err)
			}
			continue
		}
		if op.IsEdit() {
			for _, uri := range op.URIs {
				result.AddEdited(uri)
			}
		} else {
			for _, uri := range op.URIs {
				result.AddWritten(uri)
			}
		}
	}
	// Remap links for delete replacements.
	operations.ResolvedLinks = RemapStoredLinks(operations.ResolvedLinks, operations.DeleteReplacements)
	// Inherit deleted link relations.
	if err := u.inheritDeletedLinkRelations(ctx, &operations, result, requestCtx); err != nil {
		// Non-fatal: log via result.
		result.AddError("inherit_links", err)
	}
	// Apply deletes, skipping any URI that was just upserted in this batch.
	upserted := make(map[string]struct{}, len(result.WrittenURIs)+len(result.EditedURIs))
	for _, uri := range result.WrittenURIs {
		upserted[uri] = struct{}{}
	}
	for _, uri := range result.EditedURIs {
		upserted[uri] = struct{}{}
	}
	for _, fc := range operations.DeleteFileContents {
		if _, ok := upserted[fc.URI]; ok {
			continue
		}
		if err := u.applyDelete(ctx, fc.URI, requestCtx); err != nil {
			result.AddError(fc.URI, err)
			continue
		}
		result.AddDeleted(fc.URI)
	}
	// Apply links to endpoint files not covered by upserts.
	if len(operations.ResolvedLinks) > 0 {
		updated, err := WriteStoredLinks(ctx, operations.ResolvedLinks, fs, requestCtx, upserted)
		if err != nil {
			result.AddError("links", err)
		}
		for _, uri := range updated {
			result.AddEdited(uri)
		}
	}
	// Generate overviews for touched directories.
	dirs := make(map[string]string, len(operations.UpsertOperations)+len(operations.DeleteFileContents))
	for _, op := range operations.UpsertOperations {
		for _, uri := range op.URIs {
			dir := directoryOf(uri)
			if dir != "" {
				dirs[dir] = op.MemoryType
			}
		}
	}
	for _, fc := range operations.DeleteFileContents {
		dir := directoryOf(fc.URI)
		if dir != "" {
			mt := fc.MemoryType
			if mt == "" {
				if v, ok := fc.ExtraFields["memory_type"].(string); ok {
					mt = v
				}
			}
			if mt == "" {
				mt = "unknown"
			}
			dirs[dir] = mt
		}
	}
	for dir, mt := range dirs {
		if err := u.GenerateOverview(ctx, mt, dir, requestCtx, ec); err != nil {
			// Overview failures are non-fatal.
			_ = err
		}
	}
	return result, nil
}

// applyUpsert applies one upsert operation. It mirrors
// MemoryUpdater._apply_upsert.
func (u *memoryUpdater) applyUpsert(ctx context.Context, op ResolvedOperation, requestCtx any, ec *ExtractContext) error {
	u.mu.Lock()
	fs := u.fs
	registry := u.registry
	u.mu.Unlock()
	schema, ok := registry.Get(op.MemoryType)
	if !ok {
		return fmt.Errorf("memory schema not found: %s", op.MemoryType)
	}
	for _, uri := range op.URIs {
		oldContent := readExistingFile(ctx, fs, uri, requestCtx)
		if oldContent == nil {
			oldContent = op.OldMemoryFileContent
		}
		metadata := make(map[string]any, len(op.MemoryFields)+4)
		for k, v := range op.MemoryFields {
			metadata[k] = v
		}
		if op.Source != nil && op.Source.ExtractionID != "" {
			metadata["source_extraction_id"] = op.Source.ExtractionID
		}
		if op.Source != nil && op.Source.TraceID != "" {
			metadata["last_update_trace_id"] = op.Source.TraceID
		}
		// Apply merge_op field-by-field.
		for _, field := range schema.Fields {
			patchValue, hasPatch := op.MemoryFields[field.Name]
			if !hasPatch {
				continue
			}
			var currentValue any
			if oldContent != nil {
				if field.Name == "content" {
					currentValue = oldContent.PlainContent()
				} else {
					currentValue = oldContent.ExtraFields[field.Name]
				}
			}
			mergeOp := NewMergeOpFactory().FromField(field)
			metadata[field.Name] = mergeOp.Apply(currentValue, patchValue)
		}
		// Preserve system-managed metadata from the old file.
		if oldContent != nil && oldContent.ExtraFields != nil {
			schemaFieldNames := make(map[string]struct{}, len(schema.Fields)+2)
			for _, f := range schema.Fields {
				schemaFieldNames[f.Name] = struct{}{}
			}
			schemaFieldNames["content"] = struct{}{}
			schemaFieldNames["memory_type"] = struct{}{}
			for k, v := range oldContent.ExtraFields {
				if _, ok := schemaFieldNames[k]; ok {
					continue
				}
				if _, written := metadata[k]; written {
					continue
				}
				if v != nil {
					metadata[k] = v
				}
			}
		}
		metadata["version"] = NextMemoryVersion(oldContent)
		mf := FromParsed(uri, metadata)
		content := MemoryFileWrite(mf, derefStr(schema.ContentTemplate))
		if err := fs.WriteFile(ctx, uri, content, requestCtx); err != nil {
			return fmt.Errorf("write %s: %w", uri, err)
		}
	}
	return nil
}

// applyDelete removes a memory file. It mirrors MemoryUpdater._apply_delete.
func (u *memoryUpdater) applyDelete(ctx context.Context, uri string, requestCtx any) error {
	u.mu.Lock()
	fs := u.fs
	u.mu.Unlock()
	if fs == nil {
		return errors.New("MemoryFS not configured")
	}
	if err := fs.Rm(ctx, uri, false, requestCtx); err != nil {
		// Idempotent: deleting a non-existent file is not an error.
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("delete %s: %w", uri, err)
	}
	return nil
}

// GenerateOverview writes a .overview.md file for a directory based on
// the schema's overview_template. It mirrors MemoryUpdater.generate_overview.
func (u *memoryUpdater) GenerateOverview(ctx context.Context, memoryType, directory string, requestCtx any, ec *ExtractContext) error {
	u.mu.Lock()
	fs := u.fs
	registry := u.registry
	u.mu.Unlock()
	if fs == nil || registry == nil {
		return nil
	}
	schema, ok := registry.Get(memoryType)
	if !ok || schema.OverviewTemplate == nil {
		return nil
	}
	entries, err := fs.Ls(ctx, directory, "agent", 256, true, 1000, requestCtx)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		return err
	}
	baseURI := strings.TrimRight(directory, "/")
	mdFiles := make([]string, 0, len(entries))
	for _, e := range entries {
		name, _ := e["name"].(string)
		if name == "" {
			continue
		}
		if !strings.HasSuffix(name, ".md") {
			continue
		}
		if strings.HasSuffix(name, ".overview.md") || strings.HasSuffix(name, ".abstract.md") {
			continue
		}
		mdFiles = append(mdFiles, baseURI+"/"+name)
	}
	overviewPath := baseURI + "/.overview.md"
	if len(mdFiles) == 0 {
		_ = fs.Rm(ctx, overviewPath, false, requestCtx)
		return nil
	}
	items := make([]map[string]any, 0, len(mdFiles))
	for _, fileURI := range mdFiles {
		content, err := fs.ReadFile(ctx, fileURI, requestCtx)
		if err != nil || content == "" {
			continue
		}
		mf := MemoryFileRead(content, fileURI)
		filename := fileURI
		if idx := strings.LastIndex(fileURI, "/"); idx >= 0 {
			filename = fileURI[idx+1:]
		}
		items = append(items, map[string]any{
			"file_name":    filename,
			"file_content": mf.ToMetadata(),
		})
	}
	if len(items) == 0 {
		return nil
	}
	dirName := baseURI
	if idx := strings.LastIndex(baseURI, "/"); idx >= 0 {
		dirName = baseURI[idx+1:]
	}
	rendered, err := RenderTemplate(*schema.OverviewTemplate, map[string]any{
		"memory_type":    memoryType,
		"directory_name": dirName,
		"items":          items,
	})
	if err != nil {
		return err
	}
	return fs.WriteFile(ctx, overviewPath, rendered, requestCtx)
}

// inheritDeletedLinkRelations migrates links from deleted files to
// their replacements. It mirrors
// MemoryUpdater._inherit_deleted_link_relations.
func (u *memoryUpdater) inheritDeletedLinkRelations(ctx context.Context, operations *ResolvedOperations, result *MemoryUpdateResult, requestCtx any) error {
	u.mu.Lock()
	fs := u.fs
	u.mu.Unlock()
	if fs == nil || len(operations.DeleteReplacements) == 0 {
		return nil
	}
	inherited := make(map[string]map[string][]map[string]any)
	for deletedURI, replacementURI := range operations.DeleteReplacements {
		if deletedURI == "" || replacementURI == "" || deletedURI == replacementURI {
			continue
		}
		content, err := fs.ReadFile(ctx, deletedURI, requestCtx)
		if err != nil || content == "" {
			continue
		}
		deletedFile := MemoryFileRead(content, deletedURI)
		uriRemap := map[string]string{deletedURI: replacementURI}
		for _, link := range deletedFile.Links {
			remapped := RemapLinkDict(link, uriRemap)
			fromURI, _ := remapped["from_uri"].(string)
			toURI, _ := remapped["to_uri"].(string)
			if fromURI == toURI {
				continue
			}
			if fromURI != "" {
				inherited[fromURI] = ensureLinkGroups(inherited, fromURI)
				inherited[fromURI]["links"] = append(inherited[fromURI]["links"], remapped)
			}
			if toURI != "" && toURI != deletedURI {
				inherited[toURI] = ensureLinkGroups(inherited, toURI)
				inherited[toURI]["backlinks"] = append(inherited[toURI]["backlinks"], remapped)
			}
		}
		for _, link := range deletedFile.Backlinks {
			remapped := RemapLinkDict(link, uriRemap)
			fromURI, _ := remapped["from_uri"].(string)
			toURI, _ := remapped["to_uri"].(string)
			if fromURI == toURI {
				continue
			}
			if toURI != "" {
				inherited[toURI] = ensureLinkGroups(inherited, toURI)
				inherited[toURI]["backlinks"] = append(inherited[toURI]["backlinks"], remapped)
			}
			if fromURI != "" && fromURI != deletedURI {
				inherited[fromURI] = ensureLinkGroups(inherited, fromURI)
				inherited[fromURI]["links"] = append(inherited[fromURI]["links"], remapped)
			}
		}
	}
	writtenOrEdited := make(map[string]struct{}, len(result.WrittenURIs)+len(result.EditedURIs))
	for _, uri := range result.WrittenURIs {
		writtenOrEdited[uri] = struct{}{}
	}
	for _, uri := range result.EditedURIs {
		writtenOrEdited[uri] = struct{}{}
	}
	for uri, groups := range inherited {
		if _, ok := writtenOrEdited[uri]; ok {
			continue
		}
		if _, ok := operations.DeleteReplacements[uri]; ok {
			continue
		}
		content, err := fs.ReadFile(ctx, uri, requestCtx)
		if err != nil || content == "" {
			continue
		}
		mf := MemoryFileRead(content, uri)
		if len(groups["links"]) > 0 {
			mf.Links = MergeLinks(mf.Links, groups["links"])
		}
		if len(groups["backlinks"]) > 0 {
			mf.Backlinks = MergeLinks(mf.Backlinks, groups["backlinks"])
		}
		BumpMemoryVersion(&mf)
		if err := fs.WriteFile(ctx, uri, MemoryFileWrite(mf, ""), requestCtx); err != nil {
			result.AddError(uri, err)
			continue
		}
		result.AddEdited(uri)
	}
	return nil
}

// ensureLinkGroups returns the link groups for uri, creating it when absent.
func ensureLinkGroups(inherited map[string]map[string][]map[string]any, uri string) map[string][]map[string]any {
	if g, ok := inherited[uri]; ok {
		return g
	}
	g := map[string][]map[string]any{
		"links":     {},
		"backlinks": {},
	}
	inherited[uri] = g
	return g
}

// WriteStoredLinks writes StoredLinks to their endpoint files'
// links/backlinks fields. It mirrors
// openviking.session.memory.memory_updater.write_stored_links.
//
// Files listed in skipURIs are skipped (the caller handled them in the
// same write). Returns the endpoint URIs that were successfully rewritten.
func WriteStoredLinks(ctx context.Context, links []StoredLink, fs MemoryFS, requestCtx any, skipURIs map[string]struct{}) ([]string, error) {
	if len(links) == 0 || fs == nil {
		return nil, nil
	}
	skip := skipURIs
	if skip == nil {
		skip = map[string]struct{}{}
	}
	fileLinks := make(map[string]map[string][]map[string]any)
	for _, link := range links {
		if _, ok := skip[link.FromURI]; !ok {
			if _, ok := fileLinks[link.FromURI]; !ok {
				fileLinks[link.FromURI] = map[string][]map[string]any{"links": {}, "backlinks": {}}
			}
			fileLinks[link.FromURI]["links"] = append(fileLinks[link.FromURI]["links"], storedLinkToMap(link))
		}
		if _, ok := skip[link.ToURI]; !ok {
			if _, ok := fileLinks[link.ToURI]; !ok {
				fileLinks[link.ToURI] = map[string][]map[string]any{"links": {}, "backlinks": {}}
			}
			fileLinks[link.ToURI]["backlinks"] = append(fileLinks[link.ToURI]["backlinks"], storedLinkToMap(link))
		}
	}
	updated := make([]string, 0, len(fileLinks))
	for uri, groups := range fileLinks {
		content, err := fs.ReadFile(ctx, uri, requestCtx)
		if err != nil || content == "" {
			continue
		}
		mf := MemoryFileRead(content, uri)
		if len(groups["links"]) > 0 {
			mf.Links = MergeLinks(mf.Links, groups["links"])
		}
		if len(groups["backlinks"]) > 0 {
			mf.Backlinks = MergeLinks(mf.Backlinks, groups["backlinks"])
		}
		BumpMemoryVersion(&mf)
		if err := fs.WriteFile(ctx, uri, MemoryFileWrite(mf, ""), requestCtx); err != nil {
			continue
		}
		updated = append(updated, uri)
	}
	return updated, nil
}

// RemapLinkDict returns a copy of link with from_uri/to_uri rewritten
// via uriRemap. It mirrors _remap_link_dict.
func RemapLinkDict(link map[string]any, uriRemap map[string]string) map[string]any {
	out := make(map[string]any, len(link))
	for k, v := range link {
		out[k] = v
	}
	if from, ok := out["from_uri"].(string); ok {
		if remap, ok := uriRemap[from]; ok {
			out["from_uri"] = remap
		}
	}
	if to, ok := out["to_uri"].(string); ok {
		if remap, ok := uriRemap[to]; ok {
			out["to_uri"] = remap
		}
	}
	return out
}

// RemapStoredLinks returns a copy of links with from_uri/to_uri rewritten
// via uriRemap. Links where both endpoints collapse to the same URI are
// dropped. It mirrors remap_stored_links.
func RemapStoredLinks(links []StoredLink, uriRemap map[string]string) []StoredLink {
	if len(links) == 0 || len(uriRemap) == 0 {
		out := make([]StoredLink, len(links))
		copy(out, links)
		return out
	}
	out := make([]StoredLink, 0, len(links))
	for _, link := range links {
		fromURI := link.FromURI
		if remap, ok := uriRemap[fromURI]; ok {
			fromURI = remap
		}
		toURI := link.ToURI
		if remap, ok := uriRemap[toURI]; ok {
			toURI = remap
		}
		if fromURI == toURI {
			continue
		}
		link.FromURI = fromURI
		link.ToURI = toURI
		out = append(out, link)
	}
	return out
}

// storedLinkToMap converts a StoredLink to the map form used by MergeLinks.
func storedLinkToMap(link StoredLink) map[string]any {
	m := map[string]any{
		"from_uri":   link.FromURI,
		"to_uri":     link.ToURI,
		"link_type":  link.LinkType,
		"weight":     link.Weight,
		"created_at": link.CreatedAt,
	}
	if link.MatchText != nil {
		m["match_text"] = *link.MatchText
	}
	if link.Description != "" {
		m["description"] = link.Description
	}
	return m
}

// readExistingFile reads a file and returns its MemoryFile representation,
// or nil when the file does not exist or cannot be parsed.
func readExistingFile(ctx context.Context, fs MemoryFS, uri string, requestCtx any) *MemoryFile {
	if fs == nil {
		return nil
	}
	content, err := fs.ReadFile(ctx, uri, requestCtx)
	if err != nil || content == "" {
		return nil
	}
	mf := MemoryFileRead(content, uri)
	return &mf
}

// directoryOf returns the parent directory of uri.
func directoryOf(uri string) string {
	if idx := strings.LastIndex(uri, "/"); idx >= 0 {
		return uri[:idx]
	}
	return ""
}

// derefStr returns *s or "" when s is nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// atoiSafe parses a base-10 integer. Returns an error on invalid input.
func atoiSafe(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	neg := false
	i := 0
	if s[0] == '-' {
		neg = true
		i = 1
	}
	if i >= len(s) {
		return 0, errors.New("empty")
	}
	n := 0
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, errors.New("invalid")
		}
		n = n*10 + int(s[i]-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}

// parseCompactTimestamp parses an ISO-8601 timestamp and returns the
// YYYYMMDDHHMMSS form. Returns "" on parse failure.
func parseCompactTimestamp(s string) string {
	layouts := []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("20060102150405")
		}
	}
	return ""
}

// splitTextForExtraction splits a long text into chunks at sentence
// boundaries. It mirrors ExtractContext._split_text_for_extraction.
func splitTextForExtraction(text string) []string {
	const minChunkChars = 100
	if len(text) <= minChunkChars {
		return []string{text}
	}
	units := splitTextUnits(text)
	packed := packTextUnits(units, minChunkChars)
	if len(packed) == 0 {
		return []string{text}
	}
	return packed
}

// splitTextUnits splits text into units at sentence/punctuation boundaries.
// It mirrors ExtractContext._split_text_units.
func splitTextUnits(text string) []string {
	units := make([]string, 0, 8)
	current := strings.Builder{}
	runes := []rune(text)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		current.WriteRune(r)
		if isSentenceBoundary(r) {
			units = append(units, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		units = append(units, current.String())
	}
	if len(units) == 0 {
		return []string{text}
	}
	return units
}

// packTextUnits packs units into chunks of at least minChunkChars.
// It mirrors ExtractContext._pack_text_units.
func packTextUnits(units []string, minChunkChars int) []string {
	chunks := make([]string, 0, len(units))
	current := strings.Builder{}
	for _, unit := range units {
		current.WriteString(unit)
		if current.Len() >= minChunkChars {
			chunks = append(chunks, current.String())
			current.Reset()
		}
	}
	if current.Len() > 0 {
		if len(chunks) > 0 {
			chunks[len(chunks)-1] += current.String()
		} else {
			chunks = append(chunks, current.String())
		}
	}
	return chunks
}

// isSentenceBoundary reports whether r is a sentence-ending punctuation.
func isSentenceBoundary(r rune) bool {
	switch r {
	case '\n', '。', '！', '？', '；', '!', '?', ';', '.':
		return true
	}
	return false
}

// TextMessage is a concrete Message implementation used by the extraction
// pipeline when splitting long messages into chunks. It mirrors the
// chunk_message construction in ExtractContext._split_message_for_extraction.
type TextMessage struct {
	id        string
	role      string
	peerID    string
	createdAt string
	parts     []MessagePart
}

// NewTextMessage returns a TextMessage.
func NewTextMessage(id, role, peerID, createdAt string, parts []MessagePart) *TextMessage {
	return &TextMessage{id: id, role: role, peerID: peerID, createdAt: createdAt, parts: parts}
}

// ID returns the message ID.
func (m *TextMessage) ID() string { return m.id }

// Role returns the message role.
func (m *TextMessage) Role() string { return m.role }

// PeerID returns the message peer_id.
func (m *TextMessage) PeerID() string { return m.peerID }

// CreatedAt returns the message creation timestamp.
func (m *TextMessage) CreatedAt() string { return m.createdAt }

// Parts returns the message parts.
func (m *TextMessage) Parts() []MessagePart { return m.parts }
