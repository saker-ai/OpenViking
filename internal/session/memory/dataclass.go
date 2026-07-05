// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"
)

// LinkTypeDefault is the default link label used when an LLM emits an
// invalid or missing link_type. It mirrors
// openviking.session.memory.dataclass.LINK_TYPE_DEFAULT.
const LinkTypeDefault = "related_to"

// linkTypeRE enforces the lowercase snake_case shape required for
// link_type values. Up to three segments are allowed.
var linkTypeRE = regexp.MustCompile(`^[a-z]+(?:_[a-z]+){0,2}$`)

// Predefined link labels kept for compatibility in tests/call sites. It
// mirrors openviking.session.memory.dataclass.LinkType.
const (
	LinkTypeRelatedTo   = "related_to"
	LinkTypeBelongsTo   = "belongs_to"
	LinkTypeCausedBy    = "caused_by"
	LinkTypeDerivedFrom = "derived_from"
	LinkTypeContradicts = "contradicts"
	LinkTypeEvolvedFrom = "evolved_from"
)

// NormalizeLinkType coerces raw into a valid lowercase snake_case link
// type. Invalid values fall back to LinkTypeDefault. It mirrors the
// model_validator on WikiLink.
func NormalizeLinkType(raw string) string {
	if raw == "" {
		return LinkTypeDefault
	}
	normalized := strings.ToLower(strings.TrimSpace(raw))
	normalized = strings.ReplaceAll(normalized, "-", "_")
	normalized = strings.ReplaceAll(normalized, " ", "_")
	if !linkTypeRE.MatchString(normalized) {
		return LinkTypeDefault
	}
	return normalized
}

// NormalizeWeight clamps raw into the [0, 1] range. Invalid values fall
// back to 0.5. It mirrors the model_validator on WikiLink.
func NormalizeWeight(raw any) float64 {
	f, ok := asFloat(raw)
	if !ok {
		return 0.5
	}
	if f < 0 {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// WikiLink is one link emitted by the LLM during extraction, using
// temporary page_ids. It mirrors openviking.session.memory.dataclass.WikiLink.
type WikiLink struct {
	From       int     `json:"f"`
	To         int     `json:"t"`
	LinkType   string  `json:"link_type"`
	Weight     float64 `json:"weight"`
	MatchText  *string `json:"match_text"`
	Description string `json:"description"`
}

// NewWikiLink constructs a WikiLink, applying the same normalization as
// the Python model_validator.
func NewWikiLink(raw map[string]any) WikiLink {
	link := WikiLink{
		LinkType:   LinkTypeDefault,
		Weight:     0.5,
		Description: "",
	}
	if v, ok := raw["link_type"]; ok {
		if s, ok := v.(string); ok {
			link.LinkType = NormalizeLinkType(s)
		}
	}
	if v, ok := raw["weight"]; ok {
		link.Weight = NormalizeWeight(v)
	}
	if v, ok := raw["match_text"]; ok {
		switch x := v.(type) {
		case string:
			if x != "" {
				s := x
				link.MatchText = &s
			}
		case nil:
			// match_text may be nil.
		}
	}
	if v, ok := raw["description"].(string); ok {
		link.Description = v
	}
	if v, ok := raw["f"]; ok {
		if i, ok := asInt(v); ok {
			link.From = i
		}
	}
	if v, ok := raw["t"]; ok {
		if i, ok := asInt(v); ok {
			link.To = i
		}
	}
	return link
}

// StoredLink is the persisted form of a WikiLink, with URIs instead of
// page_ids. It mirrors openviking.session.memory.dataclass.StoredLink.
type StoredLink struct {
	FromURI     string  `json:"from_uri"`
	ToURI       string  `json:"to_uri"`
	LinkType    string  `json:"link_type"`
	Weight      float64 `json:"weight"`
	MatchText   *string `json:"match_text,omitempty"`
	Description string  `json:"description"`
	CreatedAt   string  `json:"created_at"`
}

// DeleteId identifies a memory page to delete, optionally remapping its
// links/backlinks to a replacement page. It mirrors
// openviking.session.memory.dataclass.DeleteId.
type DeleteId struct {
	DeletePageID       *int `json:"delete_page_id"`
	ReplacementPageID  *int `json:"replacement_page_id"`
}

// MemoryOperationSource is the runtime and persisted provenance for one
// extracted memory operation. It mirrors
// openviking.session.memory.dataclass.MemoryOperationSource.
type MemoryOperationSource struct {
	ExtractionID string
	SessionID    string
	ArchiveURI   string
	TaskID       string
	TraceID      string
	ExtractedAt  string
}

// MemoryField is one field definition on a memory type. It mirrors
// openviking.session.memory.dataclass.MemoryField.
type MemoryField struct {
	Name        string    `yaml:"name" json:"name"`
	FieldType   FieldType `yaml:"type" json:"field_type"`
	Description string    `yaml:"description" json:"description"`
	MergeOp     MergeOp   `yaml:"merge_op" json:"merge_op"`
	InitValue   *string   `yaml:"init_value,omitempty" json:"init_value,omitempty"`
}

// MemoryTypeSchema is the schema definition for one memory type. It
// mirrors openviking.session.memory.dataclass.MemoryTypeSchema.
type MemoryTypeSchema struct {
	MemoryType        string        `yaml:"memory_type" json:"memory_type"`
	Description       string        `yaml:"description" json:"description"`
	Fields            []MemoryField `yaml:"fields" json:"fields"`
	FilenameTemplate  string        `yaml:"filename_template" json:"filename_template"`
	ContentTemplate   *string       `yaml:"content_template,omitempty" json:"content_template,omitempty"`
	EmbeddingTemplate *string       `yaml:"embedding_template,omitempty" json:"embedding_template,omitempty"`
	Directory         string        `yaml:"directory" json:"directory"`
	Enabled           bool          `yaml:"enabled" json:"enabled"`
	OperationMode     string        `yaml:"operation_mode" json:"operation_mode"`
	Stage             string        `yaml:"stage" json:"stage"`
	PeerEnabled       bool          `yaml:"peer_enabled" json:"peer_enabled"`
	OverviewTemplate  *string       `yaml:"overview_template,omitempty" json:"overview_template,omitempty"`
}

// FilenameHasVariables reports whether the filename template contains
// Jinja-style variables.
func (s MemoryTypeSchema) FilenameHasVariables() bool {
	return strings.Contains(s.FilenameTemplate, "{{") && strings.Contains(s.FilenameTemplate, "}}")
}

// MemoryData is the dynamic memory data payload. It mirrors
// openviking.session.memory.dataclass.MemoryData.
type MemoryData struct {
	MemoryType string         `json:"memory_type"`
	URI        string         `json:"uri,omitempty"`
	Fields     map[string]any `json:"fields"`
	Abstract   *string        `json:"abstract,omitempty"`
	Overview   *string        `json:"overview,omitempty"`
	Content    *string        `json:"content,omitempty"`
	Name       *string        `json:"name,omitempty"`
	Tags       []string       `json:"tags"`
	CreatedAt  *time.Time     `json:"created_at,omitempty"`
	UpdatedAt  *time.Time     `json:"updated_at,omitempty"`
}

// GetField returns the field value for name, or nil.
func (d MemoryData) GetField(name string) any { return d.Fields[name] }

// SetField sets the field value for name.
func (d *MemoryData) SetField(name string, value any) {
	if d.Fields == nil {
		d.Fields = make(map[string]any)
	}
	d.Fields[name] = value
}

// MemoryFile is the typed representation of a memory file's parsed
// content. It mirrors openviking.session.memory.dataclass.MemoryFile.
type MemoryFile struct {
	URI         string
	Content     string
	Links       []map[string]any
	Backlinks   []map[string]any
	MemoryType  string
	ExtraFields map[string]any
}

// PlainContent returns the content with relative markdown links stripped
// to their text. External, anchor, and absolute-path links are
// preserved.
func (mf MemoryFile) PlainContent() string {
	return StripLinks(mf.Content)
}

// FromParsed builds a MemoryFile from parse_memory_file_with_fields
// output. It mirrors MemoryFile.from_parsed.
func FromParsed(uri string, parsed map[string]any) MemoryFile {
	if parsed == nil {
		parsed = map[string]any{}
	}
	mf := MemoryFile{URI: uri, ExtraFields: map[string]any{}}
	if v, ok := parsed["content"].(string); ok {
		mf.Content = v
		delete(parsed, "content")
	}
	if v, ok := parsed["links"].([]map[string]any); ok {
		mf.Links = v
		delete(parsed, "links")
	} else if v, ok := parsed["links"].([]any); ok {
		mf.Links = anySliceToMapSlice(v)
		delete(parsed, "links")
	}
	if v, ok := parsed["backlinks"].([]map[string]any); ok {
		mf.Backlinks = v
		delete(parsed, "backlinks")
	} else if v, ok := parsed["backlinks"].([]any); ok {
		mf.Backlinks = anySliceToMapSlice(v)
		delete(parsed, "backlinks")
	}
	if v, ok := parsed["memory_type"].(string); ok {
		mf.MemoryType = v
		delete(parsed, "memory_type")
	}
	for k, v := range parsed {
		mf.ExtraFields[k] = v
	}
	return mf
}

// ToMetadata flattens the MemoryFile to a map suitable for
// serialize_with_metadata. It mirrors MemoryFile.to_metadata.
func (mf MemoryFile) ToMetadata() map[string]any {
	out := make(map[string]any, len(mf.ExtraFields))
	for k, v := range mf.ExtraFields {
		if k == "user_id" || k == "user_ids" || k == "_uri" {
			continue
		}
		out[k] = v
	}
	if _, ok := out["version"]; !ok {
		out["version"] = 1
	}
	out["content"] = mf.Content
	if len(mf.Links) > 0 {
		out["links"] = mf.Links
	}
	if len(mf.Backlinks) > 0 {
		out["backlinks"] = mf.Backlinks
	}
	if mf.MemoryType != "" {
		out["memory_type"] = mf.MemoryType
	}
	return out
}

// ResolvedOperation is one resolved memory operation ready to be
// applied. It mirrors openviking.session.memory.dataclass.ResolvedOperation.
//
// NOTE: internal/session/skill/dedup.go previously held a stub
// ResolvedOperation/ResolvedOperations; once this package landed the
// stub was replaced with a type alias to the canonical types here.
type ResolvedOperation struct {
	OldMemoryFileContent *MemoryFile
	MemoryFields          map[string]any
	MemoryType           string
	URIs                 []string
	PageID               *int
	Source               *MemoryOperationSource
}

// IsEdit reports whether this operation updates an existing memory file.
func (op ResolvedOperation) IsEdit() bool { return op.OldMemoryFileContent != nil }

// ResolvedOperations is the batch result from the session memory
// pipeline. It mirrors openviking.session.memory.dataclass.ResolvedOperations.
type ResolvedOperations struct {
	UpsertOperations   []ResolvedOperation
	DeleteFileContents []MemoryFile
	Errors             []string
	ResolvedLinks      []StoredLink
	DeleteReplacements map[string]string
}

// HasErrors reports whether any errors were collected during resolution.
func (o ResolvedOperations) HasErrors() bool { return len(o.Errors) > 0 }

// StructuredMemoryOperations is the fallback memory operations model
// with fault tolerance. It mirrors
// openviking.session.memory.dataclass.StructuredMemoryOperations.
type StructuredMemoryOperations struct {
	Reasoning string         `json:"reasoning"`
	WriteURIs []map[string]any `json:"write_uris"`
	EditURIs  []map[string]any `json:"edit_uris"`
	DeleteIDs []DeleteId     `json:"delete_ids"`
}

// IsEmpty reports whether there are no operations.
func (o StructuredMemoryOperations) IsEmpty() bool {
	return len(o.WriteURIs) == 0 && len(o.EditURIs) == 0 && len(o.DeleteIDs) == 0
}

// ToLegacyOperations returns the legacy {write_uris, edit_uris, delete_ids}
// view. Identity for the fallback model.
func (o StructuredMemoryOperations) ToLegacyOperations() map[string]any {
	return map[string]any{
		"write_uris":  o.WriteURIs,
		"edit_uris":   o.EditURIs,
		"delete_ids":  o.DeleteIDs,
	}
}

// anySliceToMapSlice coerces []any into []map[string]any, dropping any
// elements that are not maps.
func anySliceToMapSlice(in []any) []map[string]any {
	out := make([]map[string]any, 0, len(in))
	for _, v := range in {
		if m, ok := v.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// asInt coerces v into an int. The second return is false on
// non-coercible input.
func asInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case int32:
		return int(x), true
	case int64:
		return int(x), true
	case float32:
		return int(x), true
	case float64:
		return int(x), true
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return int(i), true
		}
	case string:
		// Used by line-number extraction and wiki-link parsing.
		i := 0
		neg := false
		s := x
		if len(s) > 0 && s[0] == '-' {
			neg = true
			s = s[1:]
		}
		if len(s) == 0 {
			return 0, false
		}
		for _, c := range []byte(s) {
			if c < '0' || c > '9' {
				return 0, false
			}
			i = i*10 + int(c-'0')
		}
		if neg {
			i = -i
		}
		return i, true
	}
	return 0, false
}
