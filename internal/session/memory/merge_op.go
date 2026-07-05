// Copyright (c) 2026 Beijing Volcano Engine Technology Co., Ltd.
// SPDX-License-Identifier: AGPL-3.0

package memory

// PatchOp is the SEARCH/REPLACE merge operation for string fields and a
// direct replacement for non-string fields. It mirrors
// openviking.session.memory.merge_op.patch.PatchOp.
type PatchOp struct {
	fieldType FieldType
}

// NewPatchOp returns a PatchOp for the given field type.
func NewPatchOp(ft FieldType) *PatchOp { return &PatchOp{fieldType: ft} }

// OpType returns MergeOpPatch.
func (op *PatchOp) OpType() MergeOp { return MergeOpPatch }

// OutputSchemaType returns "str_patch" for string fields, otherwise the
// field type's JSON-schema type.
func (op *PatchOp) OutputSchemaType(ft FieldType) string {
	if ft == FieldTypeString {
		return "str_patch"
	}
	return FieldTypeToSchema(ft)
}

// OutputSchemaDescription returns the description the LLM sees.
func (op *PatchOp) OutputSchemaDescription(desc string) string {
	if op.fieldType == FieldTypeString {
		return "PATCH operation for '" + desc + "'. Follow the shared SEARCH/REPLACE rules above."
	}
	return "Replace value for '" + desc + "'"
}

// Apply applies patchValue to currentValue.
//
// For non-string fields it is a direct replacement. For string fields:
//
//   - When currentValue is nil (no original content), the replace content
//     of the first SEARCH/REPLACE block is used directly.
//   - When currentValue is a string and patchValue is a StrPatch or a
//     {blocks:[...]} map, apply_str_patch is invoked. Blocks with empty
//     search are filtered out.
//   - When patchValue is a plain string, it replaces the current value
//     unless it is empty.
func (op *PatchOp) Apply(currentValue, patchValue any) any {
	if op.fieldType != FieldTypeString {
		return patchValue
	}

	if currentValue == nil {
		return extractReplaceWhenNoOriginal(patchValue)
	}

	currentStr, _ := currentValue.(string)

	switch pv := patchValue.(type) {
	case StrPatch:
		valid := filterNonEmptySearch(pv.Blocks)
		if len(valid) > 0 {
			out, err := ApplyStrPatch(currentStr, StrPatch{Blocks: valid})
			if err == nil {
				return out
			}
		}
		return currentValue
	case map[string]any:
		blocks, ok := pv["blocks"].([]any)
		if !ok {
			// Fall back to simple replacement.
			if s, ok := pv["replace"].(string); ok && s != "" {
				return s
			}
			return currentValue
		}
		parsed := make([]SearchReplaceBlock, 0, len(blocks))
		for _, b := range blocks {
			bm, ok := b.(map[string]any)
			if !ok {
				continue
			}
			search, _ := bm["search"].(string)
			replace, _ := bm["replace"].(string)
			parsed = append(parsed, SearchReplaceBlock{Search: search, Replace: replace})
		}
		valid := filterNonEmptySearch(parsed)
		if len(valid) > 0 {
			out, err := ApplyStrPatch(currentStr, StrPatch{Blocks: valid})
			if err == nil {
				return out
			}
		}
		return currentValue
	case string:
		if pv == "" {
			return currentValue
		}
		return pv
	}
	return currentValue
}

// extractReplaceWhenNoOriginal mirrors PatchOp._extract_replace_when_no_original.
func extractReplaceWhenNoOriginal(patchValue any) any {
	switch pv := patchValue.(type) {
	case StrPatch:
		return pv.FirstReplace()
	case map[string]any:
		if blocks, ok := pv["blocks"].([]any); ok && len(blocks) > 0 {
			if bm, ok := blocks[0].(map[string]any); ok {
				if r, ok := bm["replace"].(string); ok {
					return r
				}
			}
		}
	case string:
		return pv
	}
	return ""
}

// filterNonEmptySearch drops blocks whose search text is empty. Used by
// PatchOp.Apply to skip invalid patches against non-empty content.
func filterNonEmptySearch(blocks []SearchReplaceBlock) []SearchReplaceBlock {
	out := make([]SearchReplaceBlock, 0, len(blocks))
	for _, b := range blocks {
		if b.Search == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

// ReplaceOp is the full-replacement merge operation. It mirrors
// openviking.session.memory.merge_op.replace.ReplaceOp.
type ReplaceOp struct{}

// NewReplaceOp returns a ReplaceOp.
func NewReplaceOp() *ReplaceOp { return &ReplaceOp{} }

// OpType returns MergeOpReplace.
func (op *ReplaceOp) OpType() MergeOp { return MergeOpReplace }

// OutputSchemaType returns the field type's JSON-schema type.
func (op *ReplaceOp) OutputSchemaType(ft FieldType) string { return FieldTypeToSchema(ft) }

// OutputSchemaDescription returns the description the LLM sees.
func (op *ReplaceOp) OutputSchemaDescription(desc string) string {
	return "Full replacement for '" + desc + "'. " +
		"Output the complete new content as a plain string. " +
		"You must have read the current content first and incorporate it."
}

// Apply replaces currentValue with patchValue unless patchValue is nil or
// empty.
func (op *ReplaceOp) Apply(currentValue, patchValue any) any {
	if patchValue == nil {
		return currentValue
	}
	if s, ok := patchValue.(string); ok && s == "" {
		return currentValue
	}
	return patchValue
}

// SumOp is the numeric addition merge operation. It mirrors
// openviking.session.memory.merge_op.sum.SumOp.
type SumOp struct{}

// NewSumOp returns a SumOp.
func NewSumOp() *SumOp { return &SumOp{} }

// OpType returns MergeOpSum.
func (op *SumOp) OpType() MergeOp { return MergeOpSum }

// OutputSchemaType returns "integer" by default (matches the Python
// default=int).
func (op *SumOp) OutputSchemaType(ft FieldType) string {
	if ft == FieldTypeFloat32 {
		return "number"
	}
	return "integer"
}

// OutputSchemaDescription returns the description the LLM sees.
func (op *SumOp) OutputSchemaDescription(desc string) string {
	return "add for '" + desc + "'"
}

// Apply adds patchValue to currentValue. Either side may be int, int64,
// float64, or a numeric string. On type-mismatch the current value is
// preserved.
func (op *SumOp) Apply(currentValue, patchValue any) any {
	if patchValue == nil {
		return currentValue
	}
	if s, ok := patchValue.(string); ok && s == "" {
		return currentValue
	}
	if currentValue == nil {
		return patchValue
	}

	curFloat, curIsFloat := asFloat(currentValue)
	patchFloat, patchIsFloat := asFloat(patchValue)
	if !curIsFloat || !patchIsFloat {
		return currentValue
	}

	if isFloatValue(currentValue) || isFloatValue(patchValue) {
		return curFloat + patchFloat
	}
	return int64(curFloat + patchFloat)
}

// ImmutableOp is the merge operation that allows a field to be set once
// and never modified again. It mirrors
// openviking.session.memory.merge_op.immutable.ImmutableOp.
type ImmutableOp struct{}

// NewImmutableOp returns an ImmutableOp.
func NewImmutableOp() *ImmutableOp { return &ImmutableOp{} }

// OpType returns MergeOpImmutable.
func (op *ImmutableOp) OpType() MergeOp { return MergeOpImmutable }

// OutputSchemaType returns the field type's JSON-schema type.
func (op *ImmutableOp) OutputSchemaType(ft FieldType) string { return FieldTypeToSchema(ft) }

// OutputSchemaDescription returns the description the LLM sees.
func (op *ImmutableOp) OutputSchemaDescription(desc string) string {
	return "Immutable field '" + desc + "' - can only be set once, cannot be modified"
}

// Apply returns patchValue when currentValue is nil, otherwise keeps the
// existing value.
func (op *ImmutableOp) Apply(currentValue, patchValue any) any {
	if currentValue == nil {
		return patchValue
	}
	return currentValue
}

// MergeOpFactory creates MergeOpBase instances from MergeOp enum values.
// It mirrors openviking.session.memory.merge_op.factory.MergeOpFactory.
type MergeOpFactory struct{}

// NewMergeOpFactory returns a MergeOpFactory.
func NewMergeOpFactory() *MergeOpFactory { return &MergeOpFactory{} }

// Create returns the MergeOpBase for the given (op, fieldType) pair.
func (f *MergeOpFactory) Create(op MergeOp, fieldType FieldType) MergeOpBase {
	switch op {
	case MergeOpPatch:
		return NewPatchOp(fieldType)
	case MergeOpReplace:
		return NewReplaceOp()
	case MergeOpSum:
		return NewSumOp()
	case MergeOpImmutable:
		return NewImmutableOp()
	default:
		return NewPatchOp(fieldType)
	}
}

// FromField returns the MergeOpBase for a MemoryField.
func (f *MergeOpFactory) FromField(field MemoryField) MergeOpBase {
	return f.Create(field.MergeOp, field.FieldType)
}

// asFloat coerces v to a float64. The second return is false when v is
// not a recognised numeric type.
func asFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case float32:
		return float64(x), true
	case float64:
		return x, true
	case string:
		// SumOp tolerates numeric strings.
		// We avoid strconv here to keep the helper dependency-free; the
		// caller treats a false return as "preserve current value".
		return parseFloatString(x)
	}
	return 0, false
}

// isFloatValue reports whether v should be treated as a float result.
func isFloatValue(v any) bool {
	switch v.(type) {
	case float32, float64:
		return true
	case string:
		// Numeric strings with a '.' are treated as floats.
		s, _ := v.(string)
		for i := 0; i < len(s); i++ {
			if s[i] == '.' || s[i] == 'e' || s[i] == 'E' {
				return true
			}
		}
	}
	return false
}

// parseFloatString is a minimal float parser used by SumOp. It is
// intentionally conservative: it returns (0, false) for any input it
// cannot unambiguously classify as a base-10 number.
func parseFloatString(s string) (float64, bool) {
	if s == "" || s == "None" {
		return 0, false
	}
	neg := false
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		neg = s[i] == '-'
		i++
	}
	var integer float64
	sawDigit := false
	for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
		integer = integer*10 + float64(s[i]-'0')
		sawDigit = true
	}
	var fraction float64
	div := 1.0
	if i < len(s) && s[i] == '.' {
		i++
		for ; i < len(s) && s[i] >= '0' && s[i] <= '9'; i++ {
			fraction = fraction*10 + float64(s[i]-'0')
			div *= 10
			sawDigit = true
		}
	}
	if !sawDigit {
		return 0, false
	}
	out := integer + fraction/div
	if neg {
		out = -out
	}
	return out, true
}

// MergeLinks merges two link lists with dedup and conflict resolution.
// Dedup key: from_uri + to_uri + match_text. Weight conflicts take the
// max; link_type and description are latest-wins. It mirrors
// openviking.session.memory.merge_op.link_merge.merge_links.
func MergeLinks(existing, incoming []map[string]any) []map[string]any {
	merged := make(map[string]map[string]any)
	order := make([]string, 0, len(existing)+len(incoming))

	add := func(link map[string]any) {
		key := linkDedupKey(link)
		if cur, ok := merged[key]; ok {
			curWeight, _ := cur["weight"].(float64)
			newWeight, _ := link["weight"].(float64)
			if newWeight > curWeight {
				cur["weight"] = newWeight
			}
			if v, ok := link["link_type"]; ok {
				cur["link_type"] = v
			}
			if v, ok := link["description"]; ok {
				cur["description"] = v
			}
			return
		}
		clone := make(map[string]any, len(link))
		for k, v := range link {
			clone[k] = v
		}
		merged[key] = clone
		order = append(order, key)
	}

	for _, l := range existing {
		add(l)
	}
	for _, l := range incoming {
		add(l)
	}

	out := make([]map[string]any, 0, len(order))
	for _, k := range order {
		out = append(out, merged[k])
	}
	return out
}

// linkDedupKey returns the dedup signature for a link.
func linkDedupKey(link map[string]any) string {
	from, _ := link["from_uri"].(string)
	to, _ := link["to_uri"].(string)
	match, _ := link["match_text"].(string)
	return from + "|" + to + "|" + match
}
