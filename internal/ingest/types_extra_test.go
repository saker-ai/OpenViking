package ingest

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestValueEqual verifies valueEqual across all supported type branches.
func TestValueEqual(t *testing.T) {
	// int64 == int64
	assert.True(t, valueEqual(int64(5), int64(5)))
	assert.False(t, valueEqual(int64(5), int64(6)))

	// int == int
	assert.True(t, valueEqual(int(3), int(3)))
	assert.False(t, valueEqual(int(3), int(4)))
	// int == int64 (cross-type numeric)
	assert.True(t, valueEqual(int(3), int64(3)))
	assert.False(t, valueEqual(int(3), int64(4)))

	// float64 == float64
	assert.True(t, valueEqual(float64(1.5), float64(1.5)))
	assert.False(t, valueEqual(float64(1.5), float64(2.5)))

	// string == string
	assert.True(t, valueEqual("a", "a"))
	assert.False(t, valueEqual("a", "b"))

	// uint64 == uint64
	assert.True(t, valueEqual(uint64(7), uint64(7)))
	assert.False(t, valueEqual(uint64(7), uint64(8)))
	// uint64 == int64 (cross-type)
	assert.True(t, valueEqual(uint64(7), int64(7)))
	assert.False(t, valueEqual(uint64(7), int64(8)))

	// incompatible types fall through to == and return false.
	assert.False(t, valueEqual(true, "x"))
	// bool vs bool falls through to == comparison.
	assert.True(t, valueEqual(true, true))
	assert.False(t, valueEqual(true, false))
}

// TestCursorEqual_DifferentValueLen verifies cursors with different value
// map lengths are not equal.
func TestCursorEqual_DifferentValueLen(t *testing.T) {
	a := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(0)}}
	b := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(0), "extra": "x"}}
	assert.False(t, a.Equal(b))
}

// TestCursorEqual_MissingKey verifies a key present in one but not the
// other returns not equal.
func TestCursorEqual_MissingKey(t *testing.T) {
	a := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(0)}}
	b := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"other": int64(0)}}
	assert.False(t, a.Equal(b))
}

// TestCursorEqual_ValueTypes verifies Equal handles all cursor value types
// via valueEqual.
func TestCursorEqual_ValueTypes(t *testing.T) {
	// string values
	a := &Cursor{Kind: CursorRowIDTime, Value: map[string]any{"time": int64(0), "id": "abc"}}
	b := &Cursor{Kind: CursorRowIDTime, Value: map[string]any{"time": int64(0), "id": "abc"}}
	assert.True(t, a.Equal(b))

	// different string value
	c := &Cursor{Kind: CursorRowIDTime, Value: map[string]any{"time": int64(0), "id": "xyz"}}
	assert.False(t, a.Equal(c))
}
