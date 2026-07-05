package domain

import "time"

// ResourceType enumerates the kinds of entities storable in ragfs.
type ResourceType string

const (
	ResourceTypeFile     ResourceType = "file"
	ResourceTypeDir      ResourceType = "dir"
	ResourceTypeURL      ResourceType = "url"
	ResourceTypeMemory   ResourceType = "memory"
	ResourceTypeSkill    ResourceType = "skill"
	ResourceTypeSession  ResourceType = "session"
	ResourceTypeRelation ResourceType = "relation"
)

// LayerInfo records the presence of L0/L1/L2 hidden files for a resource.
type LayerInfo struct {
	HasAbstract bool `json:"has_abstract"`
	HasOverview bool `json:"has_overview"`
	ChunkCount  int  `json:"chunk_count"`
	FallbackL1  bool `json:"fallback_l1,omitempty"`
}

// Resource is the domain model for anything addressable by a viking:// URI.
type Resource struct {
	URI        string         `json:"uri"`
	Type       ResourceType   `json:"type"`
	Name       string         `json:"name"`
	Parent     string         `json:"parent,omitempty"`
	MimeType   string         `json:"mime_type,omitempty"`
	Size       int64          `json:"size,omitempty"`
	Hash       string         `json:"hash,omitempty"`
	CreatedAt  time.Time      `json:"created_at"`
	ModifiedAt time.Time      `json:"modified_at"`
	Owner      Identifier     `json:"owner"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Layers     LayerInfo      `json:"layers"`
}

// IsDir reports whether the resource is a directory.
func (r *Resource) IsDir() bool { return r.Type == ResourceTypeDir }
