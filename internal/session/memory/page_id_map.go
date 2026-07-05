// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

// PageIdMap is the temporary page_id -> URI mapping for one ExtractLoop
// lifecycle. It mirrors openviking.session.memory.page_id_map.PageIdMap.
//
// Existing pages (from prefetch/read) get IDs 1-99; new pages (from LLM
// output) get IDs 100+. A URI can have multiple page_ids pointing to it
// (e.g. an existing page_id=1 from prefetch plus an LLM-declared
// page_id=100 when editing the same page). Both IDs resolve to the same
// URI.
type PageIdMap struct {
	nextID    int
	idToURI   map[int]string
	uriToID   map[string]int
}

// NewPageIdMap returns an empty PageIdMap.
func NewPageIdMap() *PageIdMap {
	return &PageIdMap{
		nextID:  1,
		idToURI: make(map[int]string),
		uriToID: make(map[string]int),
	}
}

// GetPageID registers an existing page (from prefetch/read) and returns
// its page_id. Re-registering the same URI returns the existing id.
func (m *PageIdMap) GetPageID(uri string) int {
	if id, ok := m.uriToID[uri]; ok {
		return id
	}
	id := m.nextID
	m.nextID++
	m.idToURI[id] = uri
	m.uriToID[uri] = id
	return id
}

// RegisterNewPageID registers an LLM-declared page_id for uri. If uri
// was already registered, the existing id mapping is left intact.
func (m *PageIdMap) RegisterNewPageID(uri string, pageID int) {
	m.idToURI[pageID] = uri
	if _, ok := m.uriToID[uri]; !ok {
		m.uriToID[uri] = pageID
	}
}

// Resolve returns the URI for pageID, or "" when not registered.
func (m *PageIdMap) Resolve(pageID int) string {
	return m.idToURI[pageID]
}

// HasLinksEnabled reports whether any pages have been registered (the
// links feature is active).
func (m *PageIdMap) HasLinksEnabled() bool {
	return len(m.idToURI) > 0
}
