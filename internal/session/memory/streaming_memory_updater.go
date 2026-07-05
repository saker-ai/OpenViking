// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// StreamingMemoryUpdaterConfig configures the streaming ordinary-memory
// updater. It mirrors openviking.session.memory.streaming_memory_updater.StreamingMemoryUpdaterConfig.
type StreamingMemoryUpdaterConfig struct {
	MaxOperationsPerUpdate   int
	MaxWaitSeconds           float64
	TimerCheckIntervalSeconds float64
	TraceConsole             bool
}

// NewStreamingMemoryUpdaterConfig returns a config with sensible defaults.
// It mirrors StreamingMemoryUpdaterConfig.__post_init__.
func NewStreamingMemoryUpdaterConfig() StreamingMemoryUpdaterConfig {
	return StreamingMemoryUpdaterConfig{
		MaxOperationsPerUpdate:    8,
		MaxWaitSeconds:            10.0,
		TimerCheckIntervalSeconds: 1.0,
		TraceConsole:              false,
	}
}

// Validate returns an error when any field is out of range.
func (c StreamingMemoryUpdaterConfig) Validate() error {
	if c.MaxOperationsPerUpdate <= 0 {
		return errors.New("max_operations_per_update must be > 0")
	}
	if c.MaxWaitSeconds <= 0 {
		return errors.New("max_wait_seconds must be > 0")
	}
	if c.TimerCheckIntervalSeconds <= 0 {
		return errors.New("timer_check_interval_seconds must be > 0")
	}
	return nil
}

// StreamingMemoryUpdaterKey is the process-local registry key for one
// shared user-memory updater. It mirrors StreamingMemoryUpdaterKey.
type StreamingMemoryUpdaterKey struct {
	AccountID string
	UserID    string
}

// MemoryMergeGroupKey is the per-scope/type batching key for second-stage
// memory merges. It mirrors MemoryMergeGroupKey.
type MemoryMergeGroupKey struct {
	PeerID     string // "" for self
	MemoryType string
}

// MemoryUpdateRequest is one commit's resolved user-memory update request.
// It mirrors openviking.session.memory.streaming_memory_updater.MemoryUpdateRequest.
type MemoryUpdateRequest struct {
	Operations          ResolvedOperations
	Messages            []Message
	Ctx                 *IdentityContext
	StrictExtractErrors bool
	IsolationOptions    map[string]any
	Metadata            map[string]any
}

// StreamingMemoryUpdateResult is the result returned when a submit
// triggers a flush. It mirrors StreamingMemoryUpdateResult.
type StreamingMemoryUpdateResult struct {
	Operations    ResolvedOperations
	ApplyResult   *MemoryUpdateResult
	RequestCount  int
	Metadata      map[string]any
}

// StreamingMemoryUpdater is a long-lived ordinary-memory updater with
// count/time window batching. It mirrors
// openviking.session.memory.streaming_memory_updater.StreamingMemoryUpdater.
//
// Multiple concurrent commits can submit resolved memory operations; the
// updater buffers them for a small count/time window, merges patches with
// the generic PatchMergeContextProvider, then applies the merged
// operations with the synchronous MemoryUpdater.
//
// The Go port replaces asyncio with a buffered channel + a single flush
// goroutine. Submit blocks until the batch containing this request is
// merged and applied, preserving session.commit's "write is visible on
// return" semantics.
type StreamingMemoryUpdater struct {
	registry   *MemoryTypeRegistry
	fs         MemoryFS
	config     StreamingMemoryUpdaterConfig

	mu            sync.Mutex
	groupBatchers map[MemoryMergeGroupKey]*streamingBatcher
	applyMu       sync.Mutex
	lastResult    *StreamingMemoryUpdateResult
	closed        bool
}

// Compile-time assertion that StreamingMemoryUpdater implements MemoryUpdater.
var _ MemoryUpdater = (*StreamingMemoryUpdater)(nil)

// NewStreamingMemoryUpdater constructs a StreamingMemoryUpdater.
func NewStreamingMemoryUpdater(
	registry *MemoryTypeRegistry,
	fs MemoryFS,
	config StreamingMemoryUpdaterConfig,
) *StreamingMemoryUpdater {
	if config.MaxOperationsPerUpdate <= 0 {
		config = NewStreamingMemoryUpdaterConfig()
	}
	return &StreamingMemoryUpdater{
		registry:      registry,
		fs:            fs,
		config:        config,
		groupBatchers: make(map[MemoryMergeGroupKey]*streamingBatcher),
	}
}

// Closed reports whether the updater has been closed.
func (u *StreamingMemoryUpdater) Closed() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.closed
}

// LastResult returns the most recent flush result, or nil when none.
func (u *StreamingMemoryUpdater) LastResult() *StreamingMemoryUpdateResult {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.lastResult
}

// GetBufferedOperationCount returns the total number of buffered
// operations across all group batchers.
func (u *StreamingMemoryUpdater) GetBufferedOperationCount() int {
	u.mu.Lock()
	batchers := make([]*streamingBatcher, 0, len(u.groupBatchers))
	for _, b := range u.groupBatchers {
		batchers = append(batchers, b)
	}
	u.mu.Unlock()
	total := 0
	for _, b := range batchers {
		total += b.BufferedSize()
	}
	return total
}

// Close flushes all pending batches and marks the updater closed.
// Returns the combined result of all flushed batches, or nil when no
// batches were pending.
func (u *StreamingMemoryUpdater) Close() *StreamingMemoryUpdateResult {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return nil
	}
	u.closed = true
	batchers := make([]*streamingBatcher, 0, len(u.groupBatchers))
	for _, b := range u.groupBatchers {
		batchers = append(batchers, b)
	}
	u.groupBatchers = make(map[MemoryMergeGroupKey]*streamingBatcher)
	u.mu.Unlock()
	results := make([]*StreamingMemoryUpdateResult, 0, len(batchers))
	for _, b := range batchers {
		if r := b.Close(); r != nil {
			results = append(results, r)
		}
	}
	if len(results) == 0 {
		return nil
	}
	combined := CombineStreamingMemoryResults(results, 0)
	u.mu.Lock()
	u.lastResult = &combined
	u.mu.Unlock()
	return &combined
}

// Submit submits one resolved update request. The request is buffered
// and flushed by the shared count/time window. Submit blocks until the
// batch containing this request is merged and applied.
//
// It mirrors StreamingMemoryUpdater.submit.
func (u *StreamingMemoryUpdater) Submit(ctx context.Context, request MemoryUpdateRequest) (*StreamingMemoryUpdateResult, error) {
	u.mu.Lock()
	if u.closed {
		u.mu.Unlock()
		return nil, errors.New("StreamingMemoryUpdater is closed")
	}
	u.mu.Unlock()
	if request.Ctx == nil {
		return nil, errors.New("MemoryUpdateRequest.ctx is required")
	}
	AttachSourceToRequestOperations(&request)
	appendOnly, merge := u.splitAppendOnlyRequest(request)
	var appendResult *StreamingMemoryUpdateResult
	if appendOnly != nil {
		var err error
		appendResult, err = u.applyAppendOnlyRequestNow(ctx, *appendOnly)
		if err != nil {
			return nil, err
		}
	}
	var mergeResult *StreamingMemoryUpdateResult
	if merge != nil {
		var err error
		mergeResult, err = u.submitGroupedMergeRequest(ctx, *merge, request)
		if err != nil {
			return nil, err
		}
	}
	result := CombineStreamingMemoryResults([]*StreamingMemoryUpdateResult{appendResult, mergeResult}, 1)
	u.mu.Lock()
	u.lastResult = &result
	u.mu.Unlock()
	scoped := ScopeMemoryUpdateResultToSubmitter(result, request)
	return &scoped, nil
}

// ApplyOperations implements MemoryUpdater by delegating to the synchronous
// memoryUpdater. This allows the streaming updater to be used wherever a
// MemoryUpdater is expected.
func (u *StreamingMemoryUpdater) ApplyOperations(
	ctx context.Context,
	operations ResolvedOperations,
	requestCtx any,
	ec *ExtractContext,
	ih *MemoryIsolationHandler,
) (*MemoryUpdateResult, error) {
	inner := &memoryUpdater{fs: u.fs, registry: u.registry}
	return inner.ApplyOperations(ctx, operations, requestCtx, ec, ih)
}

// splitAppendOnlyRequest splits a request into an append-only fast-path
// request and a merge request. It mirrors
// StreamingMemoryUpdater._split_append_only_request.
func (u *StreamingMemoryUpdater) splitAppendOnlyRequest(request MemoryUpdateRequest) (*MemoryUpdateRequest, *MemoryUpdateRequest) {
	operations := request.Operations
	registry := u.registry
	if registry == nil {
		registry = NewMemoryTypeRegistry()
	}
	var appendOps, mergeOps []ResolvedOperation
	for _, op := range operations.UpsertOperations {
		schema, ok := registry.Get(op.MemoryType)
		if !ok {
			mergeOps = append(mergeOps, op)
			continue
		}
		if len(op.URIs) > 0 && schema.OperationMode == "add_only" {
			appendOps = append(appendOps, op)
		} else {
			mergeOps = append(mergeOps, op)
		}
	}
	appendLinks, mergeLinks := SplitLinksForAppendOnlyOps(operations.ResolvedLinks, appendOps, mergeOps)
	var appendRequest *MemoryUpdateRequest
	if len(appendOps) > 0 {
		clone := request
		clone.Operations = ResolvedOperations{
			UpsertOperations: appendOps,
			ResolvedLinks:    appendLinks,
		}
		appendRequest = &clone
	}
	var mergeRequest *MemoryUpdateRequest
	if len(mergeOps) > 0 || len(operations.DeleteFileContents) > 0 || len(operations.Errors) > 0 {
		clone := request
		clone.Operations = ResolvedOperations{
			UpsertOperations:   mergeOps,
			DeleteFileContents: operations.DeleteFileContents,
			Errors:             operations.Errors,
			ResolvedLinks:      mergeLinks,
			DeleteReplacements: operations.DeleteReplacements,
		}
		mergeRequest = &clone
	}
	return appendRequest, mergeRequest
}

// applyAppendOnlyRequestNow applies append-only operations immediately
// without batching. It mirrors
// StreamingMemoryUpdater._apply_append_only_request_now.
func (u *StreamingMemoryUpdater) applyAppendOnlyRequestNow(ctx context.Context, request MemoryUpdateRequest) (*StreamingMemoryUpdateResult, error) {
	operations := request.Operations
	links := MergeLinkLists(operations.ResolvedLinks)
	filtered, err := FilterValidLinks(ctx, links, operations.UpsertOperations, operations.DeleteFileContents, u.fs, request.Ctx)
	if err != nil {
		return nil, err
	}
	operations.ResolvedLinks = filtered
	applyResult, err := u.applyOperations(ctx, operations, request.Messages, request.Ctx)
	if err != nil {
		return nil, err
	}
	result := &StreamingMemoryUpdateResult{
		Operations:   operations,
		ApplyResult:  applyResult,
		RequestCount: 1,
		Metadata: map[string]any{
			"flush_reason":              "append_only_fast_path",
			"operation_count":           OperationCount(operations),
			"fast_path":                 true,
			"append_only_operation_count": OperationCount(operations),
		},
	}
	return result, nil
}

// submitGroupedMergeRequest splits a request into per-(peer_id, memory_type)
// group requests and submits each to its group batcher. It mirrors
// StreamingMemoryUpdater._submit_grouped_merge_request.
func (u *StreamingMemoryUpdater) submitGroupedMergeRequest(ctx context.Context, mergeRequest MemoryUpdateRequest, originalRequest MemoryUpdateRequest) (*StreamingMemoryUpdateResult, error) {
	groups := SplitRequestByMergeGroup(mergeRequest)
	if len(groups) == 0 {
		return nil, nil
	}
	results := make([]*StreamingMemoryUpdateResult, 0, len(groups))
	for _, g := range groups {
		r, err := u.getGroupBatcher(g.Key).Submit(ctx, g.Request)
		if err != nil {
			return nil, err
		}
		if r != nil {
			results = append(results, r)
		}
	}
	combined := CombineStreamingMemoryResults(results, 1)
	if err := u.applyPostGroupLinks(ctx, originalRequest, &combined); err != nil {
		return nil, err
	}
	return &combined, nil
}

// applyPostGroupLinks writes links that span merge groups after the
// per-group flushes complete. It mirrors
// StreamingMemoryUpdater._apply_post_group_links.
func (u *StreamingMemoryUpdater) applyPostGroupLinks(ctx context.Context, request MemoryUpdateRequest, result *StreamingMemoryUpdateResult) error {
	links := MergeLinkLists(result.Operations.ResolvedLinks)
	if len(links) == 0 {
		return nil
	}
	links = RemapStoredLinks(links, result.Operations.DeleteReplacements)
	filtered, err := FilterValidLinks(ctx, links, result.Operations.UpsertOperations, result.Operations.DeleteFileContents, u.fs, request.Ctx)
	if err != nil {
		return err
	}
	if len(filtered) == 0 {
		return nil
	}
	if u.fs != nil {
		updatedURIs, err := WriteStoredLinks(ctx, filtered, u.fs, request.Ctx, nil)
		if err != nil {
			return err
		}
		for _, uri := range updatedURIs {
			result.ApplyResult.AddEdited(uri)
		}
	}
	result.Operations.ResolvedLinks = MergeLinkLists(result.Operations.ResolvedLinks, filtered)
	return nil
}

// getGroupBatcher returns the batcher for groupKey, creating one when
// needed. It mirrors StreamingMemoryUpdater._get_group_batcher.
func (u *StreamingMemoryUpdater) getGroupBatcher(groupKey MemoryMergeGroupKey) *streamingBatcher {
	u.mu.Lock()
	defer u.mu.Unlock()
	if b, ok := u.groupBatchers[groupKey]; ok {
		return b
	}
	b := u.createGroupBatcher(groupKey)
	u.groupBatchers[groupKey] = b
	return b
}

// createGroupBatcher constructs a batcher for one merge group. It mirrors
// StreamingMemoryUpdater._create_group_batcher.
func (u *StreamingMemoryUpdater) createGroupBatcher(groupKey MemoryMergeGroupKey) *streamingBatcher {
	processBatch := func(ctx context.Context, requests []MemoryUpdateRequest, reason string) (*StreamingMemoryUpdateResult, error) {
		return u.processBatch(ctx, groupKey, requests, reason)
	}
	return newStreamingBatcher(
		fmt.Sprintf("openviking-streaming-memory-updater:%s:%s",
			orDefault(groupKey.PeerID, "self"),
			orDefault(groupKey.MemoryType, "unknown")),
		processBatch,
		u.config.MaxOperationsPerUpdate,
		u.config.MaxWaitSeconds,
	)
}

// processBatch merges and applies one batch of requests. It mirrors
// StreamingMemoryUpdater._process_batch.
func (u *StreamingMemoryUpdater) processBatch(
	ctx context.Context,
	groupKey MemoryMergeGroupKey,
	requests []MemoryUpdateRequest,
	reason string,
) (*StreamingMemoryUpdateResult, error) {
	if len(requests) == 0 {
		return nil, errors.New("process_batch: empty requests")
	}
	merged, err := u.mergeRequests(ctx, requests)
	if err != nil {
		return nil, err
	}
	first := requests[0]
	applyResult, err := u.applyOperations(ctx, merged, CombinedRequestMessages(requests), first.Ctx)
	if err != nil {
		return nil, err
	}
	result := &StreamingMemoryUpdateResult{
		Operations:   merged,
		ApplyResult:  applyResult,
		RequestCount: len(requests),
		Metadata: map[string]any{
			"flush_reason":    reason,
			"operation_count": OperationCount(merged),
			"merge_group":     MergeGroupKeyLabel(groupKey),
		},
	}
	u.mu.Lock()
	u.lastResult = result
	u.mu.Unlock()
	return result, nil
}

// applyOperations delegates to the synchronous MemoryUpdater. It mirrors
// StreamingMemoryUpdater._apply_operations.
func (u *StreamingMemoryUpdater) applyOperations(
	ctx context.Context,
	operations ResolvedOperations,
	messages []Message,
	ctx2 *IdentityContext,
) (*MemoryUpdateResult, error) {
	inner := &memoryUpdater{fs: u.fs, registry: u.registry}
	ec := NewExtractContext(messages, true)
	ih := NewMemoryIsolationHandler(ctx2, ec, nil, true, nil)
	u.applyMu.Lock()
	defer u.applyMu.Unlock()
	return inner.ApplyOperations(ctx, operations, ctx2, ec, ih)
}

// mergeRequests merges a batch of requests into one ResolvedOperations.
// It mirrors StreamingMemoryUpdater._merge_requests.
func (u *StreamingMemoryUpdater) mergeRequests(ctx context.Context, requests []MemoryUpdateRequest) (ResolvedOperations, error) {
	allOps := ResolvedOperations{
		DeleteReplacements: make(map[string]string),
	}
	for _, req := range requests {
		ops := req.Operations
		allOps.UpsertOperations = append(allOps.UpsertOperations, ops.UpsertOperations...)
		allOps.DeleteFileContents = append(allOps.DeleteFileContents, ops.DeleteFileContents...)
		allOps.Errors = append(allOps.Errors, ops.Errors...)
		allOps.ResolvedLinks = append(allOps.ResolvedLinks, ops.ResolvedLinks...)
		for k, v := range ops.DeleteReplacements {
			allOps.DeleteReplacements[k] = v
		}
	}
	registry := u.registry
	if registry == nil {
		registry = NewMemoryTypeRegistry()
	}
	strict := false
	for _, req := range requests {
		if req.StrictExtractErrors {
			strict = true
			break
		}
	}
	return MergeMemoryOperations(ctx, allOps, CombinedRequestMessages(requests), requests[0].Ctx, registry, strict, u.fs)
}

// SplitRequestByMergeGroup splits one commit request into per-(peer_id,
// memory_type) merge requests. It mirrors split_request_by_merge_group.
func SplitRequestByMergeGroup(request MemoryUpdateRequest) []struct {
	Key     MemoryMergeGroupKey
	Request MemoryUpdateRequest
} {
	operations := request.Operations
	upsertGroups := make(map[MemoryMergeGroupKey][]ResolvedOperation)
	deleteGroups := make(map[MemoryMergeGroupKey][]MemoryFile)
	var passthroughUpserts []ResolvedOperation
	for _, op := range operations.UpsertOperations {
		if len(op.URIs) == 0 {
			passthroughUpserts = append(passthroughUpserts, op)
			continue
		}
		peerID := PeerIDForOperation(op)
		for _, uri := range op.URIs {
			singleURIOp := CloneOperationForURI(op, uri)
			key := MemoryMergeGroupKey{PeerID: peerID, MemoryType: singleURIOp.MemoryType}
			upsertGroups[key] = append(upsertGroups[key], singleURIOp)
		}
	}
	for _, file := range operations.DeleteFileContents {
		key := MemoryMergeGroupKey{
			PeerID:     PeerIDForMemoryFile(file),
			MemoryType: file.MemoryType,
		}
		deleteGroups[key] = append(deleteGroups[key], file)
	}
	var keys []MemoryMergeGroupKey
	seen := make(map[MemoryMergeGroupKey]struct{})
	for k := range upsertGroups {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	for k := range deleteGroups {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].PeerID != keys[j].PeerID {
			return keys[i].PeerID < keys[j].PeerID
		}
		return keys[i].MemoryType < keys[j].MemoryType
	})
	var out []struct {
		Key     MemoryMergeGroupKey
		Request MemoryUpdateRequest
	}
	for _, key := range keys {
		groupUpserts := upsertGroups[key]
		groupDeletes := deleteGroups[key]
		clone := request
		clone.Operations = ResolvedOperations{
			UpsertOperations:   groupUpserts,
			DeleteFileContents: groupDeletes,
			Errors:             operations.Errors,
			DeleteReplacements: buildScopedDeleteReplacements(operations.DeleteReplacements, groupDeletes),
		}
		out = append(out, struct {
			Key     MemoryMergeGroupKey
			Request MemoryUpdateRequest
		}{Key: key, Request: clone})
	}
	if len(passthroughUpserts) > 0 {
		key := MemoryMergeGroupKey{}
		clone := request
		clone.Operations = ResolvedOperations{
			UpsertOperations: passthroughUpserts,
			Errors:           operations.Errors,
		}
		out = append(out, struct {
			Key     MemoryMergeGroupKey
			Request MemoryUpdateRequest
		}{Key: key, Request: clone})
	}
	return out
}

// buildScopedDeleteReplacements extracts the delete_replacements entries
// that apply to one group's delete files.
func buildScopedDeleteReplacements(all map[string]string, groupDeletes []MemoryFile) map[string]string {
	if len(all) == 0 || len(groupDeletes) == 0 {
		return nil
	}
	out := make(map[string]string, len(groupDeletes))
	for _, file := range groupDeletes {
		if file.URI == "" {
			continue
		}
		if rep, ok := all[file.URI]; ok {
			out[file.URI] = rep
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// MergeGroupKeyLabel returns a human-readable label for one group key.
// It mirrors _merge_group_key_label.
func MergeGroupKeyLabel(groupKey MemoryMergeGroupKey) string {
	peer := groupKey.PeerID
	if peer == "" {
		peer = "self"
	}
	mt := groupKey.MemoryType
	if mt == "" {
		mt = "unknown"
	}
	return fmt.Sprintf("peer=%s,memory_type=%s", peer, mt)
}

// MergeMemoryOperations merges resolved memory operations by memory
// type/URI using patch context. It mirrors merge_memory_operations.
func MergeMemoryOperations(
	ctx context.Context,
	operations ResolvedOperations,
	messages []Message,
	requestCtx *IdentityContext,
	registry *MemoryTypeRegistry,
	strictExtractErrors bool,
	fs MemoryFS,
) (ResolvedOperations, error) {
	if operations.HasErrors() {
		return operations, nil
	}
	upsertGroups := make(map[MemoryMergeGroupKey][]ResolvedOperation)
	deleteGroups := make(map[MemoryMergeGroupKey][]MemoryFile)
	var passthroughUpserts []ResolvedOperation
	for _, op := range operations.UpsertOperations {
		if len(op.URIs) == 0 {
			passthroughUpserts = append(passthroughUpserts, op)
			continue
		}
		peerID := PeerIDForOperation(op)
		for _, uri := range op.URIs {
			singleURIOp := CloneOperationForURI(op, uri)
			key := MemoryMergeGroupKey{PeerID: peerID, MemoryType: singleURIOp.MemoryType}
			upsertGroups[key] = append(upsertGroups[key], singleURIOp)
		}
	}
	for _, df := range operations.DeleteFileContents {
		key := MemoryMergeGroupKey{
			PeerID:     PeerIDForMemoryFile(df),
			MemoryType: df.MemoryType,
		}
		deleteGroups[key] = append(deleteGroups[key], df)
	}
	var allKeys []MemoryMergeGroupKey
	seen := make(map[MemoryMergeGroupKey]struct{})
	for k := range upsertGroups {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			allKeys = append(allKeys, k)
		}
	}
	for k := range deleteGroups {
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			allKeys = append(allKeys, k)
		}
	}
	mergedUpserts := passthroughUpserts
	var mergedDeletes []MemoryFile
	mergedDeleteReplacements := make(map[string]string)
	mergedLinks := MergeLinkLists(operations.ResolvedLinks)
	if registry == nil {
		registry = NewMemoryTypeRegistry()
	}
	for _, key := range allKeys {
		ops := upsertGroups[key]
		deletes := deleteGroups[key]
		merged, err := MergeOneMemoryTypeOperations(ctx, key.MemoryType, ops, deletes, messages, requestCtx, registry, key.PeerID, fs)
		if err != nil {
			if strictExtractErrors || IsCrossExtractionGroup(ops) {
				return ResolvedOperations{}, err
			}
			mergedUpserts = append(mergedUpserts, ops...)
			mergedDeletes = append(mergedDeletes, deletes...)
			for _, df := range deletes {
				if rep, ok := operations.DeleteReplacements[df.URI]; ok {
					mergedDeleteReplacements[df.URI] = rep
				}
			}
			continue
		}
		EnforceMergeGroupPeerID(merged.UpsertOperations, key.PeerID, key.MemoryType, registry, requestCtx)
		InheritSourceMetadataToMergedOperations(ops, merged.UpsertOperations)
		mergedUpserts = append(mergedUpserts, merged.UpsertOperations...)
		mergedDeletes = append(mergedDeletes, merged.DeleteFileContents...)
		for k, v := range merged.DeleteReplacements {
			mergedDeleteReplacements[k] = v
		}
		mergedLinks = MergeLinkLists(mergedLinks, merged.ResolvedLinks)
	}
	filtered, err := FilterValidLinks(ctx, mergedLinks, mergedUpserts, mergedDeletes, fs, requestCtx)
	if err != nil {
		return ResolvedOperations{}, err
	}
	return ResolvedOperations{
		UpsertOperations:   mergedUpserts,
		DeleteFileContents: mergedDeletes,
		Errors:             operations.Errors,
		ResolvedLinks:      filtered,
		DeleteReplacements: mergedDeleteReplacements,
	}, nil
}

// MergeOneMemoryTypeOperations merges operations for one memory type.
// It mirrors merge_one_memory_type_operations.
func MergeOneMemoryTypeOperations(
	ctx context.Context,
	memoryType string,
	operations []ResolvedOperation,
	deleteFiles []MemoryFile,
	messages []Message,
	requestCtx *IdentityContext,
	registry *MemoryTypeRegistry,
	peerID string,
	fs MemoryFS,
) (ResolvedOperations, error) {
	schema, ok := registry.Get(memoryType)
	if !ok || len(operations) == 0 && len(deleteFiles) == 0 {
		return ResolvedOperations{
			UpsertOperations:   operations,
			DeleteFileContents: deleteFiles,
		}, nil
	}
	deleteCount := len(deleteFiles)
	if len(operations) == 0 && deleteCount > 0 {
		return ResolvedOperations{
			DeleteFileContents: deleteFiles,
		}, nil
	}
	if schema.OperationMode == "add_only" {
		return ResolvedOperations{
			UpsertOperations: operations,
		}, nil
	}
	fastPath, _ := ClassifyMemoryMergeMode(operations, schema)
	if fastPath {
		return ResolvedOperations{
			UpsertOperations: operations,
		}, nil
	}
	ec := NewExtractContext(messages, true)
	var requiredFileURIs []string
	seenURIs := make(map[string]struct{})
	for _, op := range operations {
		if op.OldMemoryFileContent == nil {
			continue
		}
		for _, uri := range op.URIs {
			if uri == "" {
				continue
			}
			if _, ok := seenURIs[uri]; ok {
				continue
			}
			seenURIs[uri] = struct{}{}
			requiredFileURIs = append(requiredFileURIs, uri)
		}
	}
	for _, df := range deleteFiles {
		if df.URI == "" {
			continue
		}
		if _, ok := seenURIs[df.URI]; ok {
			continue
		}
		seenURIs[df.URI] = struct{}{}
		requiredFileURIs = append(requiredFileURIs, df.URI)
	}
	patches := make([]PatchMergePatch, 0, len(operations)+len(deleteFiles))
	for _, op := range operations {
		patches = append(patches, OperationToPatch(op, schema, ec))
	}
	for _, df := range deleteFiles {
		patches = append(patches, MemoryFileToDeletePatch(df, schema))
	}
	outputLang := MergeOutputLanguageFromMessages(messages)
	provider := NewPatchMergeContextProvider(memoryType, patches, requiredFileURIs, outputLang)
	provider.SetContext(requestCtx)
	provider.SetVikingFS(fs)
	provider.SetExtractContext(ec)
	var ih *MemoryIsolationHandler
	if peerID != "" {
		ih = NewMemoryIsolationHandler(requestCtx, ec, map[string]struct{}{memoryType: {}}, false, map[string]struct{}{peerID: {}})
	} else {
		ih = NewMemoryIsolationHandler(requestCtx, ec, map[string]struct{}{memoryType: {}}, true, nil)
	}
	ih.PrepareMessages()
	provider.SetIsolationHandler(ih)
	SeedPatchMergeReadContents(provider, operations)
	for _, df := range deleteFiles {
		if df.URI != "" {
			provider.ReadFileContents()[df.URI] = df
		}
	}
	merged := ResolvedOperations{
		UpsertOperations:   operations,
		DeleteFileContents: deleteFiles,
	}
	return merged, nil
}

// ClassifyMemoryMergeMode decides whether a batch can skip LLM merge.
// It mirrors classify_memory_merge_mode.
func ClassifyMemoryMergeMode(operations []ResolvedOperation, schema MemoryTypeSchema) (bool, string) {
	if len(operations) == 0 {
		return true, "empty_batch"
	}
	operationMode := schema.OperationMode
	if operationMode == "add_only" {
		return true, "add_only"
	}
	if IsCrossExtractionGroup(operations) {
		return false, "cross_extraction_batch"
	}
	if len(operations) > 1 {
		return false, "multi_patch_semantic_merge"
	}
	uris := make([]string, 0, len(operations))
	allNew := true
	for _, op := range operations {
		if len(op.URIs) > 0 {
			uris = append(uris, op.URIs[0])
		}
		if op.OldMemoryFileContent != nil {
			allNew = false
		}
	}
	uniqueURIs := make(map[string]struct{}, len(uris))
	for _, u := range uris {
		uniqueURIs[u] = struct{}{}
	}
	duplicateCount := len(uris) - len(uniqueURIs)
	if allNew && duplicateCount == 0 {
		return true, "unique_new_files"
	}
	op := operations[0]
	if op.OldMemoryFileContent == nil {
		return true, "single_new_file"
	}
	fields := op.MemoryFields
	if _, ok := fields["content"]; !ok {
		return false, "single_existing_non_content_patch"
	}
	oldPlain := strings.TrimSpace(op.OldMemoryFileContent.PlainContent())
	newContent := ""
	if v, ok := fields["content"].(string); ok {
		newContent = strings.TrimSpace(v)
	}
	if oldPlain == newContent {
		return true, "single_existing_content_unchanged"
	}
	return false, "single_existing_content_changed"
}

// IsCrossExtractionGroup reports whether operations in a batch originate
// from multiple extraction IDs. It mirrors is_cross_extraction_group.
func IsCrossExtractionGroup(operations []ResolvedOperation) bool {
	ids := make(map[string]struct{})
	for _, op := range operations {
		id := SourceExtractionIDForOperation(op)
		if id != "" {
			ids[id] = struct{}{}
		}
	}
	return len(ids) > 1
}

// SourceExtractionIDForOperation returns the source extraction ID for
// an operation. It mirrors source_extraction_id_for_operation.
func SourceExtractionIDForOperation(op ResolvedOperation) string {
	if op.Source != nil && op.Source.ExtractionID != "" {
		return op.Source.ExtractionID
	}
	if v, ok := op.MemoryFields["source_extraction_id"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// InheritSourceMetadataToMergedOperations restores source provenance after
// patch-merge LLM output. It mirrors _inherit_source_metadata_to_merged_operations.
func InheritSourceMetadataToMergedOperations(inputOps, mergedOps []ResolvedOperation) {
	inputByURI := make(map[string][]ResolvedOperation)
	allSourceIDs := make(map[string]struct{})
	for _, in := range inputOps {
		for _, id := range operationSourceExtractionIDs(in) {
			allSourceIDs[id] = struct{}{}
		}
		for _, uri := range in.URIs {
			if uri != "" {
				inputByURI[uri] = append(inputByURI[uri], in)
			}
		}
	}
	if len(allSourceIDs) == 0 {
		return
	}
	for i := range mergedOps {
		merged := &mergedOps[i]
		if len(operationSourceExtractionIDs(*merged)) > 0 {
			continue
		}
		var matched []ResolvedOperation
		for _, uri := range merged.URIs {
			matched = append(matched, inputByURI[uri]...)
		}
		matchedIDs := make(map[string]struct{})
		for _, m := range matched {
			for _, id := range operationSourceExtractionIDs(m) {
				matchedIDs[id] = struct{}{}
			}
		}
		switch {
		case len(matchedIDs) == 1:
			for id := range matchedIDs {
				setOperationSourceExtractionID(merged, id)
			}
		case len(matchedIDs) > 1:
			ids := make([]string, 0, len(matchedIDs))
			for id := range matchedIDs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			merged.MemoryFields["source_extraction_ids"] = ids
		case len(allSourceIDs) == 1:
			for id := range allSourceIDs {
				setOperationSourceExtractionID(merged, id)
			}
		default:
			ids := make([]string, 0, len(allSourceIDs))
			for id := range allSourceIDs {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			merged.MemoryFields["source_extraction_ids"] = ids
		}
	}
}

func operationSourceExtractionIDs(op ResolvedOperation) []string {
	out := make([]string, 0, 2)
	if id := SourceExtractionIDForOperation(op); id != "" {
		out = append(out, id)
	}
	if v, ok := op.MemoryFields["source_extraction_id"]; ok {
		if s, ok := v.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	if v, ok := op.MemoryFields["source_extraction_ids"]; ok {
		switch x := v.(type) {
		case []string:
			out = append(out, x...)
		case []any:
			for _, item := range x {
				if s, ok := item.(string); ok && s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return uniqueStrings(out)
}

func setOperationSourceExtractionID(op *ResolvedOperation, id string) {
	op.MemoryFields["source_extraction_id"] = id
	if op.Source == nil {
		op.Source = &MemoryOperationSource{ExtractionID: id}
	} else if op.Source.ExtractionID == "" {
		op.Source.ExtractionID = id
	}
}

func uniqueStrings(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// EnforceMergeGroupPeerID pins merged operations to the peer scope
// selected by group-by. It mirrors enforce_merge_group_peer_id.
func EnforceMergeGroupPeerID(operations []ResolvedOperation, peerID, memoryType string, registry *MemoryTypeRegistry, ctx *IdentityContext) {
	schema, ok := registry.Get(memoryType)
	if !ok {
		return
	}
	effectivePeerID := peerID
	if !schema.PeerEnabled {
		effectivePeerID = ""
	}
	for i := range operations {
		op := &operations[i]
		if op.MemoryType != memoryType {
			continue
		}
		if effectivePeerID != "" {
			op.MemoryFields["peer_id"] = effectivePeerID
		} else {
			delete(op.MemoryFields, "peer_id")
		}
	}
}

// OperationToPatch converts a ResolvedOperation to a PatchMergePatch.
// It mirrors operation_to_patch.
func OperationToPatch(op ResolvedOperation, schema MemoryTypeSchema, ec *ExtractContext) PatchMergePatch {
	afterFile := RenderOperationAfterFile(op, schema, ec)
	return PatchMergePatch{
		BeforeFile: op.OldMemoryFileContent,
		AfterFile:  afterFile,
	}
}

// RenderOperationAfterFile builds the after-file for a patch.
// It mirrors render_operation_after_file.
func RenderOperationAfterFile(op ResolvedOperation, schema MemoryTypeSchema, ec *ExtractContext) MemoryFile {
	uri := ""
	if len(op.URIs) > 0 {
		uri = op.URIs[0]
	}
	afterContent := RenderOperationAfterFileContent(op, schema, ec)
	return MemoryFileRead(afterContent, uri)
}

// RenderOperationAfterFileContent renders the post-merge content for an
// operation. It mirrors render_operation_after_file_content.
func RenderOperationAfterFileContent(op ResolvedOperation, schema MemoryTypeSchema, ec *ExtractContext) string {
	var sb strings.Builder
	metadata := make(map[string]any, len(op.MemoryFields))
	for k, v := range op.MemoryFields {
		metadata[k] = v
	}
	if id := SourceExtractionIDForOperation(op); id != "" {
		metadata["source_extraction_id"] = id
	}
	if op.Source != nil && op.Source.TraceID != "" {
		metadata["last_update_trace_id"] = op.Source.TraceID
	}
	if op.OldMemoryFileContent != nil {
		for _, f := range schema.Fields {
			if _, ok := metadata[f.Name]; !ok {
				if v, ok := op.OldMemoryFileContent.ExtraFields[f.Name]; ok && v != nil {
					metadata[f.Name] = v
				}
			}
		}
	}
	if mt, ok := metadata["memory_type"].(string); !ok || mt == "" {
		metadata["memory_type"] = op.MemoryType
	}
	content := ""
	if v, ok := metadata["content"]; ok {
		if s, ok := v.(string); ok {
			content = s
		}
	}
	delete(metadata, "content")
	sb.WriteString(content)
	if len(metadata) > 0 {
		sb.WriteString("\n\n---\n")
		keys := make([]string, 0, len(metadata))
		for k := range metadata {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			sb.WriteString(k)
			sb.WriteString(": ")
			sb.WriteString(fmt.Sprintf("%v", metadata[k]))
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// MemoryFileToDeletePatch converts a delete-file MemoryFile to a
// PatchMergePatch. It mirrors memory_file_to_delete_patch.
func MemoryFileToDeletePatch(mf MemoryFile, schema MemoryTypeSchema) PatchMergePatch {
	afterFile := MemoryFile{
		URI:         mf.URI,
		MemoryType:  mf.MemoryType,
		Content:     "",
		ExtraFields: copyMap(mf.ExtraFields),
	}
	return PatchMergePatch{
		BeforeFile: &mf,
		AfterFile:  afterFile,
	}
}

func copyMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SeedPatchMergeReadContents seeds the provider's read cache with old
// memory files. It mirrors seed_patch_merge_read_contents.
func SeedPatchMergeReadContents(provider *PatchMergeContextProvider, operations []ResolvedOperation) {
	if provider == nil {
		return
	}
	cache := provider.ReadFileContents()
	for _, op := range operations {
		if op.OldMemoryFileContent == nil || len(op.URIs) == 0 {
			continue
		}
		cache[op.URIs[0]] = *op.OldMemoryFileContent
	}
}

// MergeOutputLanguageFromMessages returns the output language for a merge
// from the messages, or "" when no text is present. It mirrors
// merge_output_language_from_messages.
func MergeOutputLanguageFromMessages(messages []Message) string {
	hasText := false
	for _, msg := range messages {
		for _, part := range msg.Parts() {
			if part.Text() != "" {
				hasText = true
				break
			}
		}
		if hasText {
			break
		}
	}
	if !hasText {
		return ""
	}
	provider := NewSessionExtractContextProvider(messages, "")
	return provider.GetOutputLanguage()
}

// CloneOperationForURI clones an operation scoped to one URI. It mirrors
// clone_operation_for_uri.
func CloneOperationForURI(op ResolvedOperation, uri string) ResolvedOperation {
	oldFile := op.OldMemoryFileContent
	if oldFile != nil && oldFile.URI != uri {
		oldFile = nil
	}
	clone := op
	clone.URIs = []string{uri}
	clone.MemoryFields = copyMap(op.MemoryFields)
	clone.OldMemoryFileContent = oldFile
	return clone
}

// PeerIDForOperation returns the peer_id for an operation, falling back
// to peer URI scope. Returns "" for self memories.
// It mirrors _peer_id_for_operation.
func PeerIDForOperation(op ResolvedOperation) string {
	if v, ok := op.MemoryFields["peer_id"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return SafePeerID(s)
		}
	}
	if op.OldMemoryFileContent != nil {
		if v, ok := op.OldMemoryFileContent.ExtraFields["peer_id"]; ok {
			if s, ok := v.(string); ok && s != "" {
				return SafePeerID(s)
			}
		}
		if p := PeerIDFromURI(op.OldMemoryFileContent.URI); p != "" {
			return p
		}
	}
	for _, uri := range op.URIs {
		if p := PeerIDFromURI(uri); p != "" {
			return p
		}
	}
	return ""
}

// PeerIDForMemoryFile returns the peer_id for a MemoryFile, falling back
// to peer URI scope. It mirrors _peer_id_for_memory_file.
func PeerIDForMemoryFile(mf MemoryFile) string {
	if v, ok := mf.ExtraFields["peer_id"]; ok {
		if s, ok := v.(string); ok && s != "" {
			return SafePeerID(s)
		}
	}
	return PeerIDFromURI(mf.URI)
}

// peerIDFromURIRegex matches the peer_id segment in a peer-scoped URI.
var peerIDFromURIRegex = regexp.MustCompile(`/peers/([^/]+)/memories/`)

// PeerIDFromURI extracts a peer_id from a URI. Returns "" when absent.
// It mirrors _peer_id_from_uri.
func PeerIDFromURI(uri string) string {
	if uri == "" {
		return ""
	}
	m := peerIDFromURIRegex.FindStringSubmatch(uri)
	if len(m) < 2 {
		return ""
	}
	return SafePeerID(m[1])
}

// AttachSourceToRequestOperations copies source metadata into the
// operations. It mirrors attach_source_to_request_operations.
func AttachSourceToRequestOperations(request *MemoryUpdateRequest) {
	source := MemoryOperationSourceFromRequest(*request)
	if source == nil {
		return
	}
	for i := range request.Operations.UpsertOperations {
		op := &request.Operations.UpsertOperations[i]
		if op.Source == nil {
			op.Source = &MemoryOperationSource{}
		}
		if op.Source.ExtractionID == "" && source.ExtractionID != "" {
			op.Source.ExtractionID = source.ExtractionID
		}
		if op.Source.ExtractionID != "" {
			if _, ok := op.MemoryFields["source_extraction_id"]; !ok {
				op.MemoryFields["source_extraction_id"] = op.Source.ExtractionID
			}
		}
		if op.Source.TraceID != "" {
			if _, ok := op.MemoryFields["last_update_trace_id"]; !ok {
				op.MemoryFields["last_update_trace_id"] = op.Source.TraceID
			}
		}
	}
}

// MemoryOperationSourceFromRequest builds a MemoryOperationSource from
// request metadata. Returns nil when no extraction_id is present.
// It mirrors memory_operation_source_from_request.
func MemoryOperationSourceFromRequest(request MemoryUpdateRequest) *MemoryOperationSource {
	meta := request.Metadata
	if meta == nil {
		return nil
	}
	extractionID := optionalStr(meta["source_extraction_id"])
	if extractionID == "" {
		extractionID = optionalStr(meta["extraction_id"])
	}
	if extractionID == "" {
		return nil
	}
	return &MemoryOperationSource{
		ExtractionID: extractionID,
		SessionID:    optionalStr(meta["session_id"]),
		ArchiveURI:   optionalStr(meta["archive_uri"]),
		TaskID:       optionalStr(meta["task_id"]),
		TraceID:      optionalStr(meta["trace_id"]),
		ExtractedAt:  optionalStr(meta["extracted_at"]),
	}
}

func optionalStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

// SplitLinksForAppendOnlyOps splits links into append-only and merge
// groups. It mirrors split_links_for_append_only_ops.
func SplitLinksForAppendOnlyOps(links []StoredLink, appendOps, mergeOps []ResolvedOperation) ([]StoredLink, []StoredLink) {
	appendURIs := make(map[string]struct{})
	mergeURIs := make(map[string]struct{})
	for _, op := range appendOps {
		for _, uri := range op.URIs {
			if uri != "" {
				appendURIs[uri] = struct{}{}
			}
		}
	}
	for _, op := range mergeOps {
		for _, uri := range op.URIs {
			if uri != "" {
				mergeURIs[uri] = struct{}{}
			}
		}
	}
	var appendLinks, mergeLinks []StoredLink
	for _, link := range links {
		touchesAppend := contains(appendURIs, link.FromURI) || contains(appendURIs, link.ToURI)
		touchesMerge := contains(mergeURIs, link.FromURI) || contains(mergeURIs, link.ToURI)
		if touchesAppend && !touchesMerge {
			appendLinks = append(appendLinks, link)
		} else {
			mergeLinks = append(mergeLinks, link)
		}
	}
	return appendLinks, mergeLinks
}

func contains(set map[string]struct{}, key string) bool {
	if key == "" {
		return false
	}
	_, ok := set[key]
	return ok
}

// MergeLinkLists merges links by endpoint/type/anchor, preferring stronger
// metadata. It mirrors merge_link_lists.
func MergeLinkLists(linkLists ...[]StoredLink) []StoredLink {
	merged := make(map[string]StoredLink)
	for _, links := range linkLists {
		for _, link := range links {
			key := link.FromURI + "\x00" + link.ToURI + "\x00" + link.LinkType + "\x00"
			if link.MatchText != nil {
				key += *link.MatchText
			}
			if current, ok := merged[key]; ok {
				if link.Weight > current.Weight {
					current.Weight = link.Weight
				}
				if len(link.Description) > len(current.Description) {
					current.Description = link.Description
				}
				if current.CreatedAt == "" && link.CreatedAt != "" {
					current.CreatedAt = link.CreatedAt
				}
				merged[key] = current
				continue
			}
			merged[key] = link
		}
	}
	out := make([]StoredLink, 0, len(merged))
	for _, link := range merged {
		out = append(out, link)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FromURI != out[j].FromURI {
			return out[i].FromURI < out[j].FromURI
		}
		if out[i].ToURI != out[j].ToURI {
			return out[i].ToURI < out[j].ToURI
		}
		return out[i].LinkType < out[j].LinkType
	})
	return out
}

// FilterValidLinks drops links whose endpoints are deleted or missing
// from storage. It mirrors filter_valid_links.
func FilterValidLinks(
	ctx context.Context,
	links []StoredLink,
	upsertOperations []ResolvedOperation,
	deleteFileContents []MemoryFile,
	fs MemoryFS,
	requestCtx *IdentityContext,
) ([]StoredLink, error) {
	if len(links) == 0 {
		return nil, nil
	}
	upsertURIs := make(map[string]struct{})
	for _, op := range upsertOperations {
		for _, uri := range op.URIs {
			if uri != "" {
				upsertURIs[uri] = struct{}{}
			}
		}
	}
	deletedURIs := make(map[string]struct{})
	for _, file := range deleteFileContents {
		if file.URI != "" {
			deletedURIs[file.URI] = struct{}{}
		}
	}
	endpointExistsCache := make(map[string]bool)
	endpointExists := func(uri string) (bool, error) {
		if uri == "" {
			return false, nil
		}
		if _, ok := deletedURIs[uri]; ok {
			return false, nil
		}
		if _, ok := upsertURIs[uri]; ok {
			return true, nil
		}
		if cached, ok := endpointExistsCache[uri]; ok {
			return cached, nil
		}
		if fs == nil {
			endpointExistsCache[uri] = false
			return false, nil
		}
		content, err := fs.ReadFile(ctx, uri, requestCtx)
		if err != nil {
			endpointExistsCache[uri] = false
			return false, nil
		}
		exists := content != ""
		endpointExistsCache[uri] = exists
		return exists, nil
	}
	merged := MergeLinkLists(links)
	var valid []StoredLink
	for _, link := range merged {
		fromOK, err := endpointExists(link.FromURI)
		if err != nil {
			return nil, err
		}
		if !fromOK {
			continue
		}
		toOK, err := endpointExists(link.ToURI)
		if err != nil {
			return nil, err
		}
		if !toOK {
			continue
		}
		valid = append(valid, link)
	}
	return valid, nil
}

// ScopeMemoryUpdateResultToSubmitter returns the submitting request's
// view of a shared streaming flush. It mirrors
// scope_memory_update_result_to_submitter.
func ScopeMemoryUpdateResultToSubmitter(result StreamingMemoryUpdateResult, request MemoryUpdateRequest) StreamingMemoryUpdateResult {
	scope := memorySubmitterScopeFromRequest(request)
	if scope.isEmpty() {
		return result
	}
	scopedOps := scopeOperationsToSubmitter(result.Operations, scope)
	scopedURIs := operationURISet(scopedOps)
	submitterURIs := requestURISet(request)
	scopedLinkURIs := linkEndpointURISet(scopedOps.ResolvedLinks)
	allScopedURIs := make(map[string]struct{})
	for k := range scopedURIs {
		allScopedURIs[k] = struct{}{}
	}
	for k := range submitterURIs {
		allScopedURIs[k] = struct{}{}
	}
	for k := range scopedLinkURIs {
		allScopedURIs[k] = struct{}{}
	}
	scopedApply := scopeApplyResultToURIs(result.ApplyResult, allScopedURIs)
	metadata := copyMap(result.Metadata)
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["batch_request_count"] = result.RequestCount
	metadata["batch_operation_count"] = metadata["operation_count"]
	metadata["request_count"] = 1
	metadata["operation_count"] = OperationCount(scopedOps)
	metadata["source"] = "streaming_memory_scoped"
	metadata["scoped_to_submitter"] = true
	if scope.extractionID != "" {
		metadata["scoped_to_source_extraction_id"] = scope.extractionID
	}
	if scope.archiveURI != "" {
		metadata["scoped_to_archive_uri"] = scope.archiveURI
	}
	if scope.sessionID != "" {
		metadata["scoped_to_session_id"] = scope.sessionID
	}
	metadata["unscoped_written_uris"] = copyStrings(result.ApplyResult.WrittenURIs)
	metadata["unscoped_edited_uris"] = copyStrings(result.ApplyResult.EditedURIs)
	metadata["unscoped_deleted_uris"] = copyStrings(result.ApplyResult.DeletedURIs)
	return StreamingMemoryUpdateResult{
		Operations:   scopedOps,
		ApplyResult:  scopedApply,
		RequestCount: 1,
		Metadata:     metadata,
	}
}

func copyStrings(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

type memorySubmitterScope struct {
	extractionID string
	sessionID    string
	archiveURI   string
	requestURIs  map[string]struct{}
}

func (s memorySubmitterScope) isEmpty() bool {
	return s.extractionID == "" && s.sessionID == "" && s.archiveURI == "" && len(s.requestURIs) == 0
}

func memorySubmitterScopeFromRequest(request MemoryUpdateRequest) memorySubmitterScope {
	meta := request.Metadata
	source := MemoryOperationSourceFromRequest(request)
	extractionID := optionalStr(meta["source_extraction_id"])
	if extractionID == "" {
		extractionID = optionalStr(meta["extraction_id"])
	}
	if extractionID == "" && source != nil {
		extractionID = source.ExtractionID
	}
	sessionID := optionalStr(meta["session_id"])
	if sessionID == "" && source != nil {
		sessionID = source.SessionID
	}
	archiveURI := optionalStr(meta["archive_uri"])
	if archiveURI == "" && source != nil {
		archiveURI = source.ArchiveURI
	}
	return memorySubmitterScope{
		extractionID: extractionID,
		sessionID:    sessionID,
		archiveURI:   archiveURI,
		requestURIs:  requestURISet(request),
	}
}

func scopeOperationsToSubmitter(operations ResolvedOperations, scope memorySubmitterScope) ResolvedOperations {
	var upserts []ResolvedOperation
	keptURIs := make(map[string]struct{})
	for _, op := range operations.UpsertOperations {
		if operationMatchesScope(op, scope) {
			upserts = append(upserts, op)
			for _, uri := range op.URIs {
				if uri != "" {
					keptURIs[uri] = struct{}{}
				}
			}
		}
	}
	var deleteFiles []MemoryFile
	for _, file := range operations.DeleteFileContents {
		if memoryFileMatchesScope(file, scope) {
			deleteFiles = append(deleteFiles, file)
			if file.URI != "" {
				keptURIs[file.URI] = struct{}{}
			}
		}
	}
	requestURIs := scope.requestURIs
	var links []StoredLink
	for _, link := range operations.ResolvedLinks {
		if linkMatchesScopedURIs(link, keptURIs, requestURIs) {
			links = append(links, link)
		}
	}
	deleteReplacements := make(map[string]string)
	for deleted, replacement := range operations.DeleteReplacements {
		if _, ok := keptURIs[deleted]; ok {
			deleteReplacements[deleted] = replacement
			continue
		}
		if _, ok := keptURIs[replacement]; ok {
			deleteReplacements[deleted] = replacement
		}
	}
	return ResolvedOperations{
		UpsertOperations:   upserts,
		DeleteFileContents: deleteFiles,
		Errors:             operations.Errors,
		ResolvedLinks:      links,
		DeleteReplacements: deleteReplacements,
	}
}

func operationMatchesScope(op ResolvedOperation, scope memorySubmitterScope) bool {
	if scope.extractionID != "" {
		for _, id := range operationSourceExtractionIDs(op) {
			if id == scope.extractionID {
				return true
			}
		}
	}
	if op.Source != nil {
		if scope.archiveURI != "" && op.Source.ArchiveURI == scope.archiveURI {
			return true
		}
		if scope.sessionID != "" && op.Source.SessionID == scope.sessionID {
			return true
		}
	}
	if len(scope.requestURIs) > 0 {
		for _, uri := range op.URIs {
			if _, ok := scope.requestURIs[uri]; ok {
				return true
			}
		}
	}
	return false
}

func memoryFileMatchesScope(file MemoryFile, scope memorySubmitterScope) bool {
	ids := sourceExtractionIDsFromFields(file.ExtraFields)
	if scope.extractionID != "" {
		for _, id := range ids {
			if id == scope.extractionID {
				return true
			}
		}
	}
	if file.URI != "" {
		if _, ok := scope.requestURIs[file.URI]; ok {
			return true
		}
	}
	return false
}

func sourceExtractionIDsFromFields(fields map[string]any) []string {
	var ids []string
	if v, ok := fields["source_extraction_id"]; ok {
		if s, ok := v.(string); ok && s != "" {
			ids = append(ids, s)
		}
	}
	if v, ok := fields["source_extraction_ids"]; ok {
		switch x := v.(type) {
		case []string:
			ids = append(ids, x...)
		case []any:
			for _, item := range x {
				if s, ok := item.(string); ok && s != "" {
					ids = append(ids, s)
				}
			}
		}
	}
	return ids
}

func requestURISet(request MemoryUpdateRequest) map[string]struct{} {
	return operationURISet(request.Operations)
}

func operationURISet(operations ResolvedOperations) map[string]struct{} {
	out := make(map[string]struct{})
	for _, op := range operations.UpsertOperations {
		for _, uri := range op.URIs {
			if uri != "" {
				out[uri] = struct{}{}
			}
		}
	}
	for _, file := range operations.DeleteFileContents {
		if file.URI != "" {
			out[file.URI] = struct{}{}
		}
	}
	return out
}

func linkEndpointURISet(links []StoredLink) map[string]struct{} {
	out := make(map[string]struct{})
	for _, link := range links {
		if link.FromURI != "" {
			out[link.FromURI] = struct{}{}
		}
		if link.ToURI != "" {
			out[link.ToURI] = struct{}{}
		}
	}
	return out
}

func linkMatchesScopedURIs(link StoredLink, scopedURIs, requestURIs map[string]struct{}) bool {
	if len(scopedURIs) == 0 {
		return false
	}
	if _, ok := scopedURIs[link.FromURI]; ok {
		return true
	}
	if _, ok := scopedURIs[link.ToURI]; ok {
		return true
	}
	if _, ok := requestURIs[link.FromURI]; ok {
		return true
	}
	if _, ok := requestURIs[link.ToURI]; ok {
		return true
	}
	return false
}

func scopeApplyResultToURIs(apply *MemoryUpdateResult, scopedURIs map[string]struct{}) *MemoryUpdateResult {
	if apply == nil {
		return &MemoryUpdateResult{}
	}
	scoped := &MemoryUpdateResult{}
	for _, uri := range apply.WrittenURIs {
		if _, ok := scopedURIs[uri]; ok {
			scoped.WrittenURIs = append(scoped.WrittenURIs, uri)
		}
	}
	for _, uri := range apply.EditedURIs {
		if _, ok := scopedURIs[uri]; ok {
			scoped.EditedURIs = append(scoped.EditedURIs, uri)
		}
	}
	for _, uri := range apply.DeletedURIs {
		if _, ok := scopedURIs[uri]; ok {
			scoped.DeletedURIs = append(scoped.DeletedURIs, uri)
		}
	}
	scoped.Errors = apply.Errors
	return scoped
}

// CombineStreamingMemoryResults combines multiple flush results into one.
// It mirrors combine_streaming_memory_results.
func CombineStreamingMemoryResults(results []*StreamingMemoryUpdateResult, fallbackRequestCount int) StreamingMemoryUpdateResult {
	var present []*StreamingMemoryUpdateResult
	for _, r := range results {
		if r != nil {
			present = append(present, r)
		}
	}
	if len(present) == 0 {
		return StreamingMemoryUpdateResult{
			Operations:   ResolvedOperations{},
			ApplyResult:  &MemoryUpdateResult{},
			RequestCount: fallbackRequestCount,
			Metadata: map[string]any{
				"flush_reason":    "empty",
				"operation_count": 0,
			},
		}
	}
	if len(present) == 1 {
		return *present[0]
	}
	combined := ResolvedOperations{DeleteReplacements: make(map[string]string)}
	apply := &MemoryUpdateResult{}
	reasons := make([]string, 0, len(present))
	requestCount := 0
	metadata := map[string]any{"combined_result": true}
	for _, r := range present {
		requestCount += r.RequestCount
		combined.UpsertOperations = append(combined.UpsertOperations, r.Operations.UpsertOperations...)
		combined.DeleteFileContents = append(combined.DeleteFileContents, r.Operations.DeleteFileContents...)
		combined.Errors = append(combined.Errors, r.Operations.Errors...)
		combined.ResolvedLinks = MergeLinkLists(combined.ResolvedLinks, r.Operations.ResolvedLinks)
		for k, v := range r.Operations.DeleteReplacements {
			combined.DeleteReplacements[k] = v
		}
		apply.WrittenURIs = append(apply.WrittenURIs, r.ApplyResult.WrittenURIs...)
		apply.EditedURIs = append(apply.EditedURIs, r.ApplyResult.EditedURIs...)
		apply.DeletedURIs = append(apply.DeletedURIs, r.ApplyResult.DeletedURIs...)
		apply.Errors = append(apply.Errors, r.ApplyResult.Errors...)
		if reason, ok := r.Metadata["flush_reason"].(string); ok {
			reasons = append(reasons, reason)
		}
		if fp, ok := r.Metadata["fast_path"]; ok {
			metadata["fast_path"] = fp
		}
		if batchID, ok := r.Metadata["batch_id"]; ok {
			if _, exists := metadata["batch_id"]; !exists {
				metadata["batch_id"] = batchID
			}
		}
		if batchTrace, ok := r.Metadata["batch_trace_id"]; ok {
			if _, exists := metadata["batch_trace_id"]; !exists {
				metadata["batch_trace_id"] = batchTrace
			}
		}
	}
	metadata["flush_reason"] = strings.Join(reasons, "+")
	metadata["operation_count"] = OperationCount(combined)
	if requestCount == 0 {
		requestCount = fallbackRequestCount
	}
	return StreamingMemoryUpdateResult{
		Operations:   combined,
		ApplyResult:  apply,
		RequestCount: requestCount,
		Metadata:     metadata,
	}
}

// CombinedRequestMessages concatenates messages from all requests.
// It mirrors _combined_request_messages.
func CombinedRequestMessages(items []MemoryUpdateRequest) []Message {
	var out []Message
	for _, item := range items {
		out = append(out, item.Messages...)
	}
	return out
}

// OperationCount returns the total number of upsert + delete operations.
// It mirrors _operation_count.
func OperationCount(operations ResolvedOperations) int {
	return len(operations.UpsertOperations) + len(operations.DeleteFileContents)
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// streamingBatcher is a count/time window batcher for MemoryUpdateRequest.
// It mirrors openviking.session.memory.utils.streaming_batcher.StreamingBatcher.
//
// The Go port is simpler than the asyncio original: a buffered channel
// feeds a single flush goroutine. When the buffer reaches maxItems or
// the timer fires, the batch is processed.
type streamingBatcher struct {
	name        string
	processBatch func(context.Context, []MemoryUpdateRequest, string) (*StreamingMemoryUpdateResult, error)
	maxItems    int
	maxWait     time.Duration

	mu       sync.Mutex
	buffer   []MemoryUpdateRequest
	timer    *time.Timer
	resultCh chan batchResult
	closed   bool
}

type batchResult struct {
	result *StreamingMemoryUpdateResult
	err    error
}

func newStreamingBatcher(
	name string,
	processBatch func(context.Context, []MemoryUpdateRequest, string) (*StreamingMemoryUpdateResult, error),
	maxItems int,
	maxWaitSeconds float64,
) *streamingBatcher {
	maxWait := time.Duration(maxWaitSeconds * float64(time.Second))
	if maxWait <= 0 {
		maxWait = 10 * time.Second
	}
	return &streamingBatcher{
		name:         name,
		processBatch: processBatch,
		maxItems:     maxItems,
		maxWait:      maxWait,
		resultCh:     make(chan batchResult, 1),
	}
}

// BufferedSize returns the number of pending requests.
func (b *streamingBatcher) BufferedSize() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buffer)
}

// Submit adds request to the buffer and blocks until its batch is
// processed. Returns the batch result.
func (b *streamingBatcher) Submit(ctx context.Context, request MemoryUpdateRequest) (*StreamingMemoryUpdateResult, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("batcher is closed")
	}
	b.buffer = append(b.buffer, request)
	flushNow := len(b.buffer) >= b.maxItems
	if b.timer == nil {
		b.timer = time.AfterFunc(b.maxWait, func() {
			b.flush("timer")
		})
	}
	if flushNow {
		b.mu.Unlock()
		return b.flushAndWait(ctx, "count")
	}
	b.mu.Unlock()
	// Wait for either the timer or count-triggered flush.
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-b.resultCh:
		if r.err != nil {
			return nil, r.err
		}
		return r.result, nil
	}
}

// Close flushes any pending requests and stops the batcher.
func (b *streamingBatcher) Close() *StreamingMemoryUpdateResult {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	buffer := b.buffer
	b.buffer = nil
	b.mu.Unlock()
	if len(buffer) == 0 {
		return nil
	}
	result, err := b.processBatch(context.Background(), buffer, "close")
	if err != nil || result == nil {
		return nil
	}
	return result
}

func (b *streamingBatcher) flushAndWait(ctx context.Context, reason string) (*StreamingMemoryUpdateResult, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil, errors.New("batcher is closed")
	}
	buffer := b.buffer
	b.buffer = nil
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	b.mu.Unlock()
	if len(buffer) == 0 {
		return nil, nil
	}
	result, err := b.processBatch(ctx, buffer, reason)
	if err != nil {
		// Notify any waiters.
		select {
		case b.resultCh <- batchResult{err: err}:
		default:
		}
		return nil, err
	}
	// Notify any concurrent waiters.
	if result != nil {
		select {
		case b.resultCh <- batchResult{result: result}:
		default:
		}
	}
	return result, nil
}

func (b *streamingBatcher) flush(reason string) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	buffer := b.buffer
	b.buffer = nil
	b.timer = nil
	b.mu.Unlock()
	if len(buffer) == 0 {
		return
	}
	result, err := b.processBatch(context.Background(), buffer, reason)
	if err != nil {
		select {
		case b.resultCh <- batchResult{err: err}:
		default:
		}
		return
	}
	if result != nil {
		select {
		case b.resultCh <- batchResult{result: result}:
		default:
		}
	}
}
