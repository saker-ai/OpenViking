// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// RoleScope mirrors openviking.session.memory.memory_isolation_handler.RoleScope.
// It is the minimal role-scope descriptor consumed by SchemaModelGenerator
// when deciding whether to emit a peer_id field for a memory type.
//
// The full RoleScope type lives in memory_isolation_handler.py; the Go
// counterpart will land with the isolation handler. Until then, the
// schema generator accepts a RoleScope value constructed inline.
type RoleScope struct {
	PeerIDs []string
	UserID  string
}

// HasPeers reports whether this scope has any peer IDs.
func (r RoleScope) HasPeers() bool { return len(r.PeerIDs) > 0 }

// PeerIDList returns a comma-separated list of peer IDs, or "" when
// none are present.
func (r RoleScope) PeerIDList() string {
	if len(r.PeerIDs) == 0 {
		return ""
	}
	return strings.Join(r.PeerIDs, ", ")
}

// nonAlnumRE is the regex used to split identifiers when converting
// snake_case or kebab-case to PascalCase.
var nonAlnumRE = regexp.MustCompile(`[^a-zA-Z0-9]+`)

// ToPascalCase converts snake_case or kebab-case to PascalCase. It
// mirrors openviking.session.memory.schema_model_generator.to_pascal_case.
func ToPascalCase(s string) string {
	words := nonAlnumRE.Split(strings.TrimSpace(s), -1)
	out := strings.Builder{}
	for _, w := range words {
		if w == "" {
			continue
		}
		out.WriteString(strings.Title(w))
	}
	return out.String()
}

// SchemaModelGenerator generates type-safe JSON-schema descriptions from
// a slice of MemoryTypeSchema definitions. It mirrors
// openviking.session.memory.schema_model_generator.SchemaModelGenerator.
//
// The Python original uses pydantic.create_model to build dynamic model
// classes at runtime. The Go counterpart emits JSON-schema dicts
// directly (no runtime type creation) because:
//   - Go lacks an equivalent of pydantic.create_model
//   - The schemas are ultimately serialized to JSON for the LLM anyway
//   - Static typing makes runtime model classes unnecessary here
type SchemaModelGenerator struct {
	schemas          []MemoryTypeSchema
	templateContext  map[string]any
	flatDataSchemas  map[string]map[string]any
	operationsSchema map[string]any
	unionSchema      map[string]any
	linkEnabled      bool
}

// NewSchemaModelGenerator returns a SchemaModelGenerator. When schemas
// is a *MemoryTypeRegistry, its ListAll(true) is used. The
// templateContext is applied to field descriptions via renderDescription.
func NewSchemaModelGenerator(schemas any, templateContext map[string]any) *SchemaModelGenerator {
	g := &SchemaModelGenerator{
		templateContext: make(map[string]any),
		flatDataSchemas: make(map[string]map[string]any),
	}
	switch v := schemas.(type) {
	case *MemoryTypeRegistry:
		g.schemas = v.ListAll(true)
	case []MemoryTypeSchema:
		g.schemas = v
	case nil:
		g.schemas = nil
	default:
		g.schemas = nil
	}
	for k, val := range templateContext {
		g.templateContext[k] = val
	}
	return g
}

// renderDescription applies template substitution to a description
// string. It mirrors SchemaModelGenerator._render_description.
func (g *SchemaModelGenerator) renderDescription(desc string) string {
	if desc == "" {
		return desc
	}
	if !strings.Contains(desc, "{{") && !strings.Contains(desc, "{%") && !strings.Contains(desc, "{#") {
		return desc
	}
	out, err := RenderTemplate(desc, g.templateContext)
	if err != nil {
		return desc
	}
	return out
}

// CreateFlatDataModel returns the JSON-schema for one memory type's
// flat data model. It mirrors SchemaModelGenerator.create_flat_data_model.
//
// The Python original returns a pydantic model class; the Go counterpart
// returns a JSON-schema dict ready for serialization.
//
// The schema does NOT include a memory_type field (each type has its
// own field in the structured operations model). A page_id field is
// always required. When roleScope has peers and the schema is
// peer-enabled and has no "ranges" field, an optional peer_id field is
// added.
func (g *SchemaModelGenerator) CreateFlatDataModel(mt MemoryTypeSchema, roleScope *RoleScope) map[string]any {
	hasPeerScope := roleScope != nil && roleScope.HasPeers()
	cacheKey := mt.MemoryType
	if hasPeerScope {
		cacheKey = cacheKey + "_peer"
	}
	if cached, ok := g.flatDataSchemas[cacheKey]; ok {
		return cached
	}
	hasRanges := false
	for _, f := range mt.Fields {
		if f.Name == "ranges" {
			hasRanges = true
			break
		}
	}
	properties := make(map[string]any)
	required := make([]string, 0, 2)
	if hasPeerScope && mt.PeerEnabled && !hasRanges {
		properties["peer_id"] = map[string]any{
			"type":        "string",
			"description": fmt.Sprintf("Stable peer identity to write peer memory for. Use only when the memory describes a peer instead of the current user. Available peer_id values in this session: %s", roleScope.PeerIDList()),
		}
	}
	properties["page_id"] = map[string]any{
		"type":        "integer",
		"description": "Temporary page_id for identifying the target memory item.",
	}
	required = append(required, "page_id")
	for _, f := range mt.Fields {
		factory := NewMergeOpFactory()
		mergeOp := factory.FromField(f)
		desc := g.renderDescription(f.Description)
		if f.MergeOp == MergeOpImmutable {
			properties[f.Name] = map[string]any{
				"type":        FieldTypeToSchema(f.FieldType),
				"description": desc,
			}
			required = append(required, f.Name)
		} else {
			patchType := mergeOp.OutputSchemaType(f.FieldType)
			patchDesc := mergeOp.OutputSchemaDescription(desc)
			baseType := FieldTypeToSchema(f.FieldType)
			properties[f.Name] = map[string]any{
				"description": patchDesc,
				"anyOf": []map[string]any{
					{"type": baseType},
					{"type": patchType},
				},
			}
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
		"title":                ToPascalCase(mt.MemoryType) + "Data",
	}
	if hasPeerScope {
		schema["title"] = ToPascalCase(mt.MemoryType) + "DataPeer"
	}
	g.flatDataSchemas[cacheKey] = schema
	return schema
}

// GenerateAllModels returns a map of memory_type to flat data model
// JSON-schema. It mirrors SchemaModelGenerator.generate_all_models.
func (g *SchemaModelGenerator) GenerateAllModels() map[string]map[string]any {
	out := make(map[string]map[string]any, len(g.schemas))
	for _, mt := range g.schemas {
		out[mt.MemoryType] = g.CreateFlatDataModel(mt, nil)
	}
	return out
}

// CreateDiscriminatedUnionModel returns the JSON-schema for the
// unified MemoryData model with discriminator support. It mirrors
// SchemaModelGenerator.create_discriminated_union_model.
//
// The Python original builds a pydantic union with discriminator. The
// Go counterpart emits a oneOf schema over the per-type flat data
// models, with a "memory_type" discriminator property on each
// sub-schema. When no schemas exist, a generic fallback schema is
// returned.
func (g *SchemaModelGenerator) CreateDiscriminatedUnionModel() map[string]any {
	if g.unionSchema != nil {
		return g.unionSchema
	}
	g.GenerateAllModels()
	if len(g.schemas) == 0 {
		g.unionSchema = map[string]any{
			"type": "object",
			"properties": map[string]any{
				"memory_type": map[string]any{
					"type":        "string",
					"description": "Memory type identifier",
				},
			},
			"required":             []string{"memory_type"},
			"additionalProperties": true,
			"title":                "GenericMemoryData",
		}
		return g.unionSchema
	}
	oneOf := make([]map[string]any, 0, len(g.schemas))
	for _, mt := range g.schemas {
		flat := g.CreateFlatDataModel(mt, nil)
		props, _ := flat["properties"].(map[string]any)
		if props == nil {
			props = make(map[string]any)
		}
		props["memory_type"] = map[string]any{
			"type":  "string",
			"const": mt.MemoryType,
		}
		req, _ := flat["required"].([]string)
		reqCopy := make([]string, 0, len(req)+1)
		reqCopy = append(reqCopy, req...)
		reqCopy = append(reqCopy, "memory_type")
		flat["properties"] = props
		flat["required"] = reqCopy
		oneOf = append(oneOf, flat)
	}
	g.unionSchema = map[string]any{
		"type":  "object",
		"oneOf": oneOf,
		"title": "MemoryData",
	}
	return g.unionSchema
}

// CreateStructuredOperationsModel returns the JSON-schema for the
// structured MemoryOperations model with type-safe write operations.
// It mirrors SchemaModelGenerator.create_structured_operations_model.
//
// Each memory_type gets its own field (a list of the flat data model).
// delete_ids is added when at least one schema supports deletion
// (operation_mode != "add_only"). links is added when linkEnabled is
// true (configured by the caller; the Go generator does not read the
// openviking_cli config — pass linkEnabled explicitly via SetLinkEnabled).
func (g *SchemaModelGenerator) CreateStructuredOperationsModel(roleScope *RoleScope) map[string]any {
	if g.operationsSchema != nil {
		return g.operationsSchema
	}
	g.GenerateAllModels()
	properties := make(map[string]any)
	memoryTypeFields := make([]string, 0, len(g.schemas))
	for _, mt := range g.schemas {
		memoryTypeFields = append(memoryTypeFields, mt.MemoryType)
		flat := g.CreateFlatDataModel(mt, roleScope)
		properties[mt.MemoryType] = map[string]any{
			"type":  "array",
			"items": flat,
			"description": fmt.Sprintf("%s memories: %s (top-level field, do not nest inside other arrays)",
				mt.MemoryType, g.renderDescription(mt.Description)),
		}
	}
	hasDeletableSchema := false
	for _, mt := range g.schemas {
		if mt.OperationMode != "add_only" {
			hasDeletableSchema = true
			break
		}
	}
	if hasDeletableSchema {
		properties["delete_ids"] = map[string]any{
			"type":        "array",
			"items":       deleteIDSchema(),
			"description": "Delete operations by page_id. Each item has delete_page_id and replacement_page_id; set replacement_page_id to null for a pure delete, or to the canonical replacement page_id so existing links/backlinks are inherited.",
		}
	}
	if g.linkEnabled {
		properties["links"] = map[string]any{
			"type":        "array",
			"items":       wikiLinkSchema(),
			"description": "Links between memory pages. Follow the link rules above. Use page_ids for `f` and `t`. Use `weight` from 0 to 1 to rank competing links.",
		}
	}
	g.operationsSchema = map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
		"title":                "StructuredMemoryOperations",
		"_memory_type_fields":  memoryTypeFields,
		"_allow_empty_list_response": true,
	}
	return g.operationsSchema
}

// GetLLMJSONSchema returns the JSON schema for the structured LLM
// operations model. It mirrors SchemaModelGenerator.get_llm_json_schema.
func (g *SchemaModelGenerator) GetLLMJSONSchema(roleScope *RoleScope) map[string]any {
	return g.CreateStructuredOperationsModel(roleScope)
}

// GetMemoryDataJSONSchema returns the JSON schema for the flat memory
// data union. It mirrors SchemaModelGenerator.get_memory_data_json_schema.
func (g *SchemaModelGenerator) GetMemoryDataJSONSchema() map[string]any {
	return g.CreateDiscriminatedUnionModel()
}

// SetLinkEnabled configures whether the structured operations schema
// includes a "links" field. The Python original reads
// config.memory.link_enabled from openviking_cli.config; the Go
// counterpart decouples from that package by exposing a setter.
func (g *SchemaModelGenerator) SetLinkEnabled(enabled bool) {
	g.linkEnabled = enabled
	g.operationsSchema = nil // invalidate cache
}

// SchemaPromptGenerator incorporates schema information into LLM
// prompts. It mirrors
// openviking.session.memory.schema_model_generator.SchemaPromptGenerator.
type SchemaPromptGenerator struct {
	schemas         []MemoryTypeSchema
	templateContext map[string]any
}

// NewSchemaPromptGenerator returns a SchemaPromptGenerator. When
// schemas is a *MemoryTypeRegistry, its ListAll(true) is used.
func NewSchemaPromptGenerator(schemas any, templateContext map[string]any) *SchemaPromptGenerator {
	g := &SchemaPromptGenerator{templateContext: make(map[string]any)}
	switch v := schemas.(type) {
	case *MemoryTypeRegistry:
		g.schemas = v.ListAll(true)
	case []MemoryTypeSchema:
		g.schemas = v
	}
	for k, val := range templateContext {
		g.templateContext[k] = val
	}
	return g
}

// renderDescription applies template substitution to a description.
func (g *SchemaPromptGenerator) renderDescription(desc string) string {
	if desc == "" {
		return desc
	}
	if !strings.Contains(desc, "{{") {
		return desc
	}
	out, err := RenderTemplate(desc, g.templateContext)
	if err != nil {
		return desc
	}
	return out
}

// GenerateTypeDescriptions returns a formatted string describing all
// memory types. It mirrors SchemaPromptGenerator.generate_type_descriptions.
func (g *SchemaPromptGenerator) GenerateTypeDescriptions() string {
	var b strings.Builder
	b.WriteString("## Available Memory Types")
	sorted := make([]MemoryTypeSchema, len(g.schemas))
	copy(sorted, g.schemas)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].MemoryType < sorted[j].MemoryType })
	for _, mt := range sorted {
		b.WriteString("\n\n### ")
		b.WriteString(mt.MemoryType)
		b.WriteString("\n")
		b.WriteString(g.renderDescription(mt.Description))
		if mt.Directory != "" || mt.FilenameTemplate != "" {
			b.WriteString("\n\n**URI Format:**\n")
			if mt.Directory != "" && mt.FilenameTemplate != "" {
				b.WriteString("- URI: `")
				b.WriteString(mt.Directory)
				b.WriteString("/")
				b.WriteString(mt.FilenameTemplate)
				b.WriteString("`\n")
			} else if mt.Directory != "" {
				b.WriteString("- Directory: `")
				b.WriteString(mt.Directory)
				b.WriteString("`\n")
			} else {
				b.WriteString("- Filename: `")
				b.WriteString(mt.FilenameTemplate)
				b.WriteString("`\n")
			}
			b.WriteString("\n**Variable Substitution:**\n")
			b.WriteString("- `{{ user_space }}` -> 'default'\n")
			for _, f := range mt.Fields {
				b.WriteString("- `{{ ")
				b.WriteString(f.Name)
				b.WriteString(" }}` -> use value from fields\n")
			}
		}
		if len(mt.Fields) > 0 {
			b.WriteString("\n**Fields:**\n")
			for _, f := range mt.Fields {
				b.WriteString("- `")
				b.WriteString(f.Name)
				b.WriteString("` (")
				b.WriteString(string(f.FieldType))
				b.WriteString("): ")
				b.WriteString(g.renderDescription(f.Description))
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// GenerateFieldDescriptions returns a formatted string describing the
// fields of one memory type, or "" when the type is not found. It
// mirrors SchemaPromptGenerator.generate_field_descriptions.
//
// The Python original returns None when not found; the Go counterpart
// returns "" (the empty string) which is the more idiomatic sentinel
// for "no description".
func (g *SchemaPromptGenerator) GenerateFieldDescriptions(memoryType string) string {
	var mt *MemoryTypeSchema
	for i := range g.schemas {
		if g.schemas[i].MemoryType == memoryType {
			mt = &g.schemas[i]
			break
		}
	}
	if mt == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("### ")
	b.WriteString(mt.MemoryType)
	b.WriteString(" Fields\n")
	for _, f := range mt.Fields {
		b.WriteString("- `")
		b.WriteString(f.Name)
		b.WriteString("`: ")
		b.WriteString(g.renderDescription(f.Description))
		b.WriteString("\n")
	}
	return b.String()
}

// GetFullPromptContext returns the full prompt context including all
// schema information. It mirrors SchemaPromptGenerator.get_full_prompt_context.
func (g *SchemaPromptGenerator) GetFullPromptContext() map[string]any {
	sorted := make([]MemoryTypeSchema, len(g.schemas))
	copy(sorted, g.schemas)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].MemoryType < sorted[j].MemoryType })
	memoryTypes := make([]map[string]any, 0, len(sorted))
	for _, mt := range sorted {
		fields := make([]map[string]any, 0, len(mt.Fields))
		for _, f := range mt.Fields {
			fields = append(fields, map[string]any{
				"name":        f.Name,
				"type":        string(f.FieldType),
				"description": f.Description,
				"merge_op":    string(f.MergeOp),
			})
		}
		memoryTypes = append(memoryTypes, map[string]any{
			"memory_type": mt.MemoryType,
			"description": mt.Description,
			"fields":      fields,
		})
	}
	return map[string]any{
		"type_descriptions": g.GenerateTypeDescriptions(),
		"memory_types":      memoryTypes,
	}
}

// deleteIDSchema returns the JSON-schema for one DeleteId.
func deleteIDSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"delete_page_id": map[string]any{
				"type":        "integer",
				"description": "page_id to delete",
			},
			"replacement_page_id": map[string]any{
				"type":        "integer",
				"description": "page_id whose links/backlinks should inherit the deleted page's links, or null for a pure delete",
			},
		},
		"required":             []string{"delete_page_id"},
		"additionalProperties": false,
	}
}

// wikiLinkSchema returns the JSON-schema for one WikiLink.
func wikiLinkSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"f": map[string]any{"type": "integer", "description": "source page_id"},
			"t": map[string]any{"type": "integer", "description": "target page_id"},
			"link_type": map[string]any{
				"type":        "string",
				"description": "lowercase snake_case link label (e.g. related_to, caused_by)",
			},
			"weight": map[string]any{
				"type":        "number",
				"description": "link weight in [0, 1]",
			},
			"match_text": map[string]any{
				"type":        "string",
				"description": "optional anchor text in the source page",
			},
			"description": map[string]any{
				"type":        "string",
				"description": "human-readable explanation of the link",
			},
		},
		"required":             []string{"f", "t"},
		"additionalProperties": false,
	}
}
