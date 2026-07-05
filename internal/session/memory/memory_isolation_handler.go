// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"fmt"
	"sort"
	"strings"
)

// InternalMemoryTypes is the set of memory types managed internally
// (not subject to the user-supplied allowed-memory-types filter).
var InternalMemoryTypes = map[string]struct{}{
	"session_skills": {},
}

// SelfPeerID is the sentinel peer_id value used to represent "the current
// user" (self) rather than a stable peer. It mirrors
// openviking.session.memory.memory_isolation_handler._SELF_PEER_ID.
const SelfPeerID = "__self"

// SafePeerID normalizes a raw peer_id value. Empty values, the literal
// "__self", and values that fail basic validation all map to "". The
// Python original uses openviking.core.peer_id.safe_peer_id; the Go
// counterpart implements the same shape with a conservative allow-list.
//
// A peer_id is considered safe when:
//   - non-empty after trimming
//   - not equal to SelfPeerID (callers handle that explicitly)
//   - composed only of [a-zA-Z0-9_-.:@]+
//   - length <= 128
func SafePeerID(raw any) string {
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" || s == SelfPeerID {
		return ""
	}
	if len(s) > 128 {
		return ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') ||
			(c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') ||
			c == '_' || c == '-' || c == '.' ||
			c == ':' || c == '@') {
			return ""
		}
	}
	return s
}

// PeerUserSpace returns the user-space fragment for memory about a stable
// peer. When peerID is SelfPeerID (or empty), the user's own space is
// returned. Otherwise the peer-scoped sub-space is returned. It mirrors
// openviking.session.memory.memory_isolation_handler.peer_user_space.
func PeerUserSpace(userSpace, peerID string) string {
	if peerID == "" || peerID == SelfPeerID {
		return userSpace
	}
	return userSpace + "/peers/" + peerID
}

// IdentityContext is the minimal subset of openviking.server.identity
// .RequestContext used by the isolation handler. Concrete callers wrap
// their own RequestContext; tests use the struct directly.
type IdentityContext struct {
	UserID    string
	AccountID string
}

// MemoryIsolationHandler resolves the session memory write target for
// each operation. It mirrors
// openviking.session.memory.memory_isolation_handler.MemoryIsolationHandler.
//
// The handler decides:
//   - which memory types may be written (allows_schema)
//   - which user-space directory URIs to render for a schema
//   - which peer_id (if any) to stamp onto an operation
//   - which URIs to assign to a resolved operation
type MemoryIsolationHandler struct {
	Ctx                *IdentityContext
	ExtractCtx         *ExtractContext
	AllowedMemoryTypes map[string]struct{}
	AllowSelf          bool
	AllowedPeerIDs     map[string]struct{}
	AllowPeer          bool
}

// NewMemoryIsolationHandler constructs a handler. When allowedMemoryTypes
// is nil, all memory types are allowed. When allowSelf is false and no
// peer_ids are supplied, no write targets are produced.
func NewMemoryIsolationHandler(
	ctx *IdentityContext,
	ec *ExtractContext,
	allowedMemoryTypes map[string]struct{},
	allowSelf bool,
	allowedPeerIDs map[string]struct{},
) *MemoryIsolationHandler {
	peerIDs := make(map[string]struct{})
	for raw := range allowedPeerIDs {
		if p := SafePeerID(raw); p != "" && p != SelfPeerID {
			peerIDs[p] = struct{}{}
		}
	}
	return &MemoryIsolationHandler{
		Ctx:                ctx,
		ExtractCtx:         ec,
		AllowedMemoryTypes: allowedMemoryTypes,
		AllowSelf:          allowSelf,
		AllowedPeerIDs:     peerIDs,
		AllowPeer:          len(peerIDs) > 0,
	}
}

// PrepareMessages is a no-op hook kept for pipeline symmetry with the
// Python original. It mirrors MemoryIsolationHandler.prepare_messages.
func (h *MemoryIsolationHandler) PrepareMessages() {}

// AllowsSchema reports whether the handler permits operations on the
// given memory type. Internal memory types (session_skills) always pass.
// When AllowedMemoryTypes is nil, all types pass. It mirrors
// MemoryIsolationHandler.allows_schema.
func (h *MemoryIsolationHandler) AllowsSchema(schema MemoryTypeSchema) bool {
	if _, ok := InternalMemoryTypes[schema.MemoryType]; ok {
		return true
	}
	if h.AllowedMemoryTypes == nil {
		return true
	}
	_, ok := h.AllowedMemoryTypes[schema.MemoryType]
	return ok
}

// GetReadScope returns the RoleScope describing the user and peer spaces
// this handler can read from. It mirrors
// MemoryIsolationHandler.get_read_scope.
func (h *MemoryIsolationHandler) GetReadScope() RoleScope {
	scope := RoleScope{}
	if h.Ctx != nil && h.Ctx.UserID != "" {
		scope.UserID = h.Ctx.UserID
	}
	if h.AllowPeer {
		peerIDs := make([]string, 0, len(h.AllowedPeerIDs))
		for p := range h.AllowedPeerIDs {
			peerIDs = append(peerIDs, p)
		}
		sort.Strings(peerIDs)
		scope.PeerIDs = peerIDs
	}
	return scope
}

// FillIdentityFields stamps user_id and peer_id onto the operation's
// field map. user_id is always set when ctx has one; peer_id is set
// only when the schema allows peers and the value passes SafePeerID.
// It mirrors MemoryIsolationHandler.fill_identity_fields.
func (h *MemoryIsolationHandler) FillIdentityFields(
	itemDict map[string]any,
	roleScope RoleScope,
	schema *MemoryTypeSchema,
) {
	_ = roleScope // unused; kept for API symmetry with Python
	if h.Ctx != nil && h.Ctx.UserID != "" {
		itemDict["user_id"] = h.Ctx.UserID
	}
	delete(itemDict, "user_ids")

	if schema != nil && !schema.PeerEnabled {
		delete(itemDict, "peer_id")
		return
	}

	peerID := SafePeerID(itemDict["peer_id"])
	if peerID != "" && peerID != SelfPeerID {
		itemDict["peer_id"] = peerID
	} else {
		delete(itemDict, "peer_id")
	}
}

// RenderSchemaDirectories returns the directory URIs a schema should be
// written to under this handler's scope. Self scope produces the user's
// own directory; peer scope produces per-peer subdirectories. It mirrors
// MemoryIsolationHandler.render_schema_directories.
func (h *MemoryIsolationHandler) RenderSchemaDirectories(schema MemoryTypeSchema) []string {
	if schema.Directory == "" {
		return nil
	}
	userID := "default"
	if h.Ctx != nil && h.Ctx.UserID != "" {
		userID = h.Ctx.UserID
	}
	userSpaces := make([]string, 0, 1+len(h.AllowedPeerIDs))
	if h.AllowSelf {
		userSpaces = append(userSpaces, userID)
	}
	if h.AllowPeer && schema.PeerEnabled {
		peerIDs := make([]string, 0, len(h.AllowedPeerIDs))
		for p := range h.AllowedPeerIDs {
			peerIDs = append(peerIDs, p)
		}
		sort.Strings(peerIDs)
		for _, p := range peerIDs {
			userSpaces = append(userSpaces, PeerUserSpace(userID, p))
		}
	}
	// Dedupe preserving order.
	seen := make(map[string]struct{}, len(userSpaces))
	out := make([]string, 0, len(userSpaces))
	for _, us := range userSpaces {
		if _, ok := seen[us]; ok {
			continue
		}
		seen[us] = struct{}{}
		rendered, err := RenderTemplate(schema.Directory, map[string]any{"user_space": us})
		if err != nil {
			continue
		}
		out = append(out, rendered)
	}
	return out
}

// CanWritePeer reports whether peerID is in this handler's allowed peer
// set. It mirrors MemoryIsolationHandler._can_write_peer.
func (h *MemoryIsolationHandler) CanWritePeer(peerID string) bool {
	if !h.AllowPeer {
		return false
	}
	_, ok := h.AllowedPeerIDs[peerID]
	return ok
}

// ResolveOperationTargetID picks the write target for one operation.
// Returns "" when no target is available. It mirrors
// MemoryIsolationHandler._resolve_operation_target_id.
func (h *MemoryIsolationHandler) ResolveOperationTargetID(rawPeerID any) string {
	peerID := SafePeerID(rawPeerID)
	if peerID == SelfPeerID && h.AllowSelf {
		return SelfPeerID
	}
	if peerID != "" && h.CanWritePeer(peerID) {
		return peerID
	}
	if fallback := h.FirstTargetIDInMessages(); fallback != "" {
		return fallback
	}
	if h.AllowSelf {
		return SelfPeerID
	}
	return ""
}

// FirstTargetIDInMessages scans the extract context's messages for the
// first write-able target_id. It mirrors
// MemoryIsolationHandler._first_target_id_in_messages.
func (h *MemoryIsolationHandler) FirstTargetIDInMessages() string {
	if h.ExtractCtx == nil {
		return ""
	}
	targets := make([]string, 0)
	for _, msg := range h.ExtractCtx.Messages {
		tid := h.messageTargetID(msg)
		if tid == "" {
			continue
		}
		targets = append(targets, tid)
	}
	for _, tid := range targets {
		if tid != SelfPeerID {
			return tid
		}
	}
	if len(targets) > 0 {
		return targets[0]
	}
	return ""
}

// messageTargetID extracts the write target from one message. It mirrors
// MemoryIsolationHandler._message_target_id.
func (h *MemoryIsolationHandler) messageTargetID(msg Message) string {
	peerID := SafePeerID(msg.PeerID())
	if peerID != "" && h.CanWritePeer(peerID) {
		return peerID
	}
	if msg.PeerID() == "" && h.AllowSelf {
		return SelfPeerID
	}
	return ""
}

// CalculateMemoryURIs assigns URIs to a resolved operation. Returns an
// empty slice when the operation is not allowed or no target is
// available. It mirrors MemoryIsolationHandler.calculate_memory_uris.
func (h *MemoryIsolationHandler) CalculateMemoryURIs(
	schema MemoryTypeSchema,
	op *ResolvedOperation,
	ec *ExtractContext,
) []string {
	if !h.AllowsSchema(schema) {
		return nil
	}
	if h.Ctx == nil || h.Ctx.UserID == "" {
		return nil
	}
	userID := h.Ctx.UserID
	op.MemoryFields["user_id"] = userID

	targetIDs := make([]string, 0)
	hasRanges := false
	if v, ok := op.MemoryFields["ranges"]; ok && v != nil {
		hasRanges = true
	}

	if !schema.PeerEnabled {
		delete(op.MemoryFields, "peer_id")
		targetIDs = []string{SelfPeerID}
	} else if hasRanges {
		targetIDs = h.rangeTargets(fmt.Sprintf("%v", op.MemoryFields["ranges"]))
		delete(op.MemoryFields, "peer_id")
	} else {
		tid := h.ResolveOperationTargetID(op.MemoryFields["peer_id"])
		if tid != "" {
			targetIDs = []string{tid}
		}
		if tid == SelfPeerID {
			delete(op.MemoryFields, "peer_id")
		} else if tid != "" {
			op.MemoryFields["peer_id"] = tid
		} else {
			delete(op.MemoryFields, "peer_id")
		}
	}

	if len(targetIDs) == 0 {
		return nil
	}

	uriSet := make(map[string]struct{}, len(targetIDs))
	uris := make([]string, 0, len(targetIDs))
	baseFields := make(map[string]any, len(op.MemoryFields))
	for k, v := range op.MemoryFields {
		baseFields[k] = v
	}
	for _, tid := range targetIDs {
		fields := make(map[string]any, len(baseFields))
		for k, v := range baseFields {
			fields[k] = v
		}
		var targetUserSpace string
		if tid == SelfPeerID {
			targetUserSpace = userID
			delete(fields, "peer_id")
		} else {
			targetUserSpace = PeerUserSpace(userID, tid)
			fields["peer_id"] = tid
		}
		uri, err := GenerateURI(schema, fields, targetUserSpace)
		if err != nil {
			continue
		}
		if _, ok := uriSet[uri]; ok {
			continue
		}
		uriSet[uri] = struct{}{}
		uris = append(uris, uri)
	}
	if hasRanges {
		delete(op.MemoryFields, "peer_id")
	}
	return uris
}

// rangeTargets extracts write-target IDs from the message ranges pointed
// to by the ranges string. It mirrors MemoryIsolationHandler._range_targets.
func (h *MemoryIsolationHandler) rangeTargets(rangesStr string) []string {
	if rangesStr == "" || h.ExtractCtx == nil {
		return nil
	}
	msgRange := h.ExtractCtx.ReadMessageRanges(rangesStr)
	if msgRange == nil {
		return nil
	}
	targets := make([]string, 0)
	for _, group := range msgRange.Elements {
		for _, msg := range group {
			tid := h.messageTargetID(msg)
			if tid == "" {
				continue
			}
			targets = append(targets, tid)
		}
	}
	// Dedupe preserving order.
	seen := make(map[string]struct{}, len(targets))
	out := make([]string, 0, len(targets))
	for _, t := range targets {
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}
