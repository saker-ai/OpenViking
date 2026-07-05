// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

// FieldType is the enumeration of memory field types. It mirrors
// openviking.session.memory.merge_op.base.FieldType.
type FieldType string

const (
	FieldTypeString  FieldType = "string"
	FieldTypeInt64   FieldType = "int64"
	FieldTypeFloat32 FieldType = "float32"
	FieldTypeBool    FieldType = "bool"
)

// MergeOp is the enumeration of merge strategies for memory fields. It
// mirrors openviking.session.memory.merge_op.base.MergeOp.
type MergeOp string

const (
	MergeOpPatch     MergeOp = "patch"
	MergeOpReplace   MergeOp = "replace"
	MergeOpSum       MergeOp = "sum"
	MergeOpImmutable MergeOp = "immutable"
)

// SearchReplaceBlock is one SEARCH/REPLACE block used by the patch merge
// op. It mirrors openviking.session.memory.merge_op.base.SearchReplaceBlock.
type SearchReplaceBlock struct {
	Search  string `json:"search"`
	Replace string `json:"replace"`
}

// StrPatch is a sequence of SEARCH/REPLACE blocks applied to a string
// field. It mirrors openviking.session.memory.merge_op.base.StrPatch.
type StrPatch struct {
	Blocks []SearchReplaceBlock `json:"blocks"`
}

// FirstReplace returns the replace content of the first block, or empty
// string when there are no blocks. Used when there is no original
// content to match against.
func (p StrPatch) FirstReplace() string {
	if len(p.Blocks) == 0 {
		return ""
	}
	return p.Blocks[0].Replace
}

// MergeOpBase is the interface every merge operation implements. It
// mirrors openviking.session.memory.merge_op.base.MergeOpBase.
type MergeOpBase interface {
	// OpType returns the MergeOp enum value for this operation.
	OpType() MergeOp
	// OutputSchemaType returns the JSON-schema type identifier that the
	// LLM should emit for a field of the given field type under this
	// merge op. For string-patch ops the type is "str_patch"; for other
	// field types it is the field type itself.
	OutputSchemaType(fieldType FieldType) string
	// OutputSchemaDescription returns the human-readable description the
	// LLM sees for a field under this merge op.
	OutputSchemaDescription(fieldDescription string) string
	// Apply merges patchValue into currentValue and returns the result.
	Apply(currentValue, patchValue any) any
}

// FieldTypeToSchema maps a FieldType to its JSON-schema type identifier.
// This is the Go counterpart of
// merge_op.base._FIELD_TYPE_TO_PYTHON + get_python_type_for_field.
func FieldTypeToSchema(ft FieldType) string {
	switch ft {
	case FieldTypeString:
		return "string"
	case FieldTypeInt64:
		return "integer"
	case FieldTypeFloat32:
		return "number"
	case FieldTypeBool:
		return "boolean"
	default:
		return "string"
	}
}
