// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// MemoryTypeRegistry is the registry for memory types. It loads memory
// type definitions from YAML files and provides access to memory type
// configurations. It mirrors openviking.session.memory.memory_type_registry.MemoryTypeRegistry.
//
// Unlike the Python original, the Go registry does NOT auto-load schemas
// from the bundled prompts/templates directory at construction time.
// Callers must explicitly call LoadFromDirectory or LoadFromYAML so the
// registry stays decoupled from PromptManager/openviking_cli config
// (those packages are not yet in the Go tree). Once PromptManager lands
// in Go, a NewDefaultRegistry constructor can wire the bundled directory
// the same way the Python __init__ does.
type MemoryTypeRegistry struct {
	types map[string]MemoryTypeSchema
}

// NewMemoryTypeRegistry returns an empty MemoryTypeRegistry. The
// returned registry is ready to have schemas registered via Register,
// LoadFromYAML, or LoadFromDirectory. It mirrors MemoryTypeRegistry.__init__
// with load_schemas=False.
func NewMemoryTypeRegistry() *MemoryTypeRegistry {
	return &MemoryTypeRegistry{types: make(map[string]MemoryTypeSchema)}
}

// Register registers a memory type. It panics-with-error on duplicate
// registration to mirror the Python ValueError. It mirrors
// MemoryTypeRegistry.register.
func (r *MemoryTypeRegistry) Register(mt MemoryTypeSchema) error {
	if r == nil {
		return fmt.Errorf("MemoryTypeRegistry is nil")
	}
	if mt.MemoryType == "" {
		return fmt.Errorf("memory_type must not be empty")
	}
	if _, exists := r.types[mt.MemoryType]; exists {
		return fmt.Errorf("duplicate memory type %q", mt.MemoryType)
	}
	r.types[mt.MemoryType] = mt
	return nil
}

// Replace replaces an existing memory type, or inserts a new one when
// absent. It mirrors MemoryTypeRegistry.replace.
func (r *MemoryTypeRegistry) Replace(mt MemoryTypeSchema) {
	if r == nil {
		return
	}
	r.types[mt.MemoryType] = mt
}

// Get returns the MemoryTypeSchema for name, or ok=false when absent.
// It mirrors MemoryTypeRegistry.get.
func (r *MemoryTypeRegistry) Get(name string) (MemoryTypeSchema, bool) {
	if r == nil {
		return MemoryTypeSchema{}, false
	}
	mt, ok := r.types[name]
	return mt, ok
}

// ListAll returns all registered memory types. When includeDisabled is
// false, disabled schemas are filtered out. The returned slice is
// sorted by MemoryType for deterministic iteration. It mirrors
// MemoryTypeRegistry.list_all.
func (r *MemoryTypeRegistry) ListAll(includeDisabled bool) []MemoryTypeSchema {
	if r == nil {
		return nil
	}
	out := make([]MemoryTypeSchema, 0, len(r.types))
	for _, mt := range r.types {
		if !includeDisabled && !mt.Enabled {
			continue
		}
		out = append(out, mt)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MemoryType < out[j].MemoryType })
	return out
}

// ListNames returns the names of all registered memory types. When
// includeDisabled is false, disabled schemas are filtered out. The
// returned slice is sorted for deterministic iteration. It mirrors
// MemoryTypeRegistry.list_names.
func (r *MemoryTypeRegistry) ListNames(includeDisabled bool) []string {
	if r == nil {
		return nil
	}
	mts := r.ListAll(includeDisabled)
	out := make([]string, 0, len(mts))
	for _, mt := range mts {
		out = append(out, mt.MemoryType)
	}
	return out
}

// ListSearchURIs returns the directory URIs for the search scope,
// rendered for the given user space. It mirrors
// MemoryTypeRegistry.list_search_uris.
func (r *MemoryTypeRegistry) ListSearchURIs(userSpace string) []string {
	if r == nil {
		return nil
	}
	if userSpace == "" {
		userSpace = "default"
	}
	uris := make([]string, 0)
	for _, mt := range r.ListAll(false) {
		if mt.Directory == "" {
			continue
		}
		rendered, err := RenderTemplate(mt.Directory, map[string]any{"user_space": userSpace})
		if err != nil {
			continue
		}
		uris = append(uris, rendered)
	}
	return uris
}

// LoadFromYAML loads a memory type from a YAML file. When replace is
// true, an existing type with the same name is overwritten; otherwise
// duplicate registration returns an error. It mirrors
// MemoryTypeRegistry.load_from_yaml.
func (r *MemoryTypeRegistry) LoadFromYAML(yamlPath string, replace bool) error {
	if r == nil {
		return fmt.Errorf("MemoryTypeRegistry is nil")
	}
	data, err := os.ReadFile(yamlPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", yamlPath, err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse %s: %w", yamlPath, err)
	}
	mt, err := ParseMemoryType(raw)
	if err != nil {
		return fmt.Errorf("parse memory type %s: %w", yamlPath, err)
	}
	if replace {
		r.Replace(mt)
		return nil
	}
	return r.Register(mt)
}

// LoadFromDirectory loads all *.yaml and *.yml files from a directory.
// When replace is true, existing types are overwritten; otherwise
// duplicate registration returns an error which is logged and skipped
// (matching the Python "best-effort" load semantics). Returns the
// number of schemas successfully loaded. It mirrors
// MemoryTypeRegistry.load_from_directory.
func (r *MemoryTypeRegistry) LoadFromDirectory(dirPath string, replace bool) int {
	if r == nil {
		return 0
	}
	info, err := os.Stat(dirPath)
	if err != nil || !info.IsDir() {
		return 0
	}
	count := 0
	for _, ext := range []string{".yaml", ".yml"} {
		matches, _ := filepath.Glob(filepath.Join(dirPath, "*"+ext))
		sort.Strings(matches)
		for _, p := range matches {
			if err := r.LoadFromYAML(p, replace); err != nil {
				continue
			}
			count++
		}
	}
	return count
}

// ParseMemoryType parses a memory type definition from a YAML-decoded
// map. It mirrors MemoryTypeRegistry._parse_memory_type.
//
// Field type and merge_op default to "string" and "patch" respectively,
// matching the Python defaults. The "enabled" flag accepts either
// "enabled" or "enable" (Python legacy compat) and defaults to true.
func ParseMemoryType(data map[string]any) (MemoryTypeSchema, error) {
	if data == nil {
		return MemoryTypeSchema{}, fmt.Errorf("empty memory type definition")
	}
	mt := MemoryTypeSchema{
		Enabled:       true,
		OperationMode: "upsert",
		Stage:         "user",
		PeerEnabled:   true,
	}
	if v, ok := data["memory_type"].(string); ok && v != "" {
		mt.MemoryType = v
	} else if v, ok := data["name"].(string); ok && v != "" {
		mt.MemoryType = v
	} else {
		return MemoryTypeSchema{}, fmt.Errorf("memory_type (or name) is required")
	}
	if v, ok := data["description"].(string); ok {
		mt.Description = v
	}
	if v, ok := data["filename_template"].(string); ok {
		mt.FilenameTemplate = v
	}
	if v, ok := data["content_template"].(string); ok {
		s := v
		mt.ContentTemplate = &s
	}
	if v, ok := data["embedding_template"].(string); ok {
		s := v
		mt.EmbeddingTemplate = &s
	}
	if v, ok := data["directory"].(string); ok {
		mt.Directory = v
	}
	if v, ok := data["operation_mode"].(string); ok && v != "" {
		mt.OperationMode = v
	}
	if v, ok := data["stage"].(string); ok && v != "" {
		mt.Stage = v
	}
	if v, ok := data["overview_template"].(string); ok {
		s := v
		mt.OverviewTemplate = &s
	}
	// enabled: accept either "enabled" or "enable" (Python legacy).
	if v, ok := data["enabled"]; ok {
		mt.Enabled = asBoolDefault(v, true)
	} else if v, ok := data["enable"]; ok {
		mt.Enabled = asBoolDefault(v, true)
	}
	if v, ok := data["peer_enabled"]; ok {
		mt.PeerEnabled = asBoolDefault(v, true)
	}
	// Fields
	if fieldsRaw, ok := data["fields"].([]any); ok {
		mt.Fields = make([]MemoryField, 0, len(fieldsRaw))
		for _, fr := range fieldsRaw {
			fm, ok := fr.(map[string]any)
			if !ok {
				continue
			}
			mt.Fields = append(mt.Fields, parseMemoryField(fm))
		}
	}
	return mt, nil
}

// parseMemoryField parses one field definition from a YAML-decoded map.
func parseMemoryField(fm map[string]any) MemoryField {
	f := MemoryField{
		FieldType: FieldTypeString,
		MergeOp:   MergeOpPatch,
	}
	if v, ok := fm["name"].(string); ok {
		f.Name = v
	}
	if v, ok := fm["type"].(string); ok && v != "" {
		switch strings.ToLower(v) {
		case "string":
			f.FieldType = FieldTypeString
		case "int64", "int":
			f.FieldType = FieldTypeInt64
		case "float32", "float", "number":
			f.FieldType = FieldTypeFloat32
		case "bool", "boolean":
			f.FieldType = FieldTypeBool
		default:
			f.FieldType = FieldTypeString
		}
	}
	if v, ok := fm["field_type"].(string); ok && v != "" {
		f.FieldType = FieldType(v)
	}
	if v, ok := fm["description"].(string); ok {
		f.Description = v
	}
	if v, ok := fm["merge_op"].(string); ok && v != "" {
		f.MergeOp = MergeOp(v)
	}
	if v, ok := fm["init_value"]; ok {
		if s, ok := v.(string); ok {
			if s != "" {
				ss := s
				f.InitValue = &ss
			}
		}
	}
	return f
}

// asBoolDefault coerces v to a bool, returning dflt when v is not a
// recognized bool type.
func asBoolDefault(v any, dflt bool) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(x) {
		case "true", "yes", "1":
			return true
		case "false", "no", "0":
			return false
		}
	case int:
		return x != 0
	case int64:
		return x != 0
	case float64:
		return x != 0
	}
	return dflt
}
