package ingest

import "testing"

func TestZeroCursor_ByteOffset(t *testing.T) {
	c := ZeroCursor(CursorByteOffset)
	if c == nil {
		t.Fatal("ZeroCursor returned nil")
	}
	if c.Kind != CursorByteOffset {
		t.Errorf("Kind = %q, want %q", c.Kind, CursorByteOffset)
	}
	if c.Offset() != 0 {
		t.Errorf("Offset = %d, want 0", c.Offset())
	}
}

func TestZeroCursor_RowIDTime(t *testing.T) {
	c := ZeroCursor(CursorRowIDTime)
	if c == nil {
		t.Fatal("ZeroCursor returned nil")
	}
	if c.Kind != CursorRowIDTime {
		t.Errorf("Kind = %q, want %q", c.Kind, CursorRowIDTime)
	}
	timeVal, ok := c.Value["time"]
	if !ok {
		t.Fatal("RowIDTime cursor missing 'time' key")
	}
	if timeVal != int64(0) {
		t.Errorf("time = %v, want 0", timeVal)
	}
}

func TestZeroCursor_UnknownKind(t *testing.T) {
	c := ZeroCursor("unknown")
	if c == nil {
		t.Fatal("ZeroCursor returned nil")
	}
	// Unknown kinds default to byte_offset style.
	if c.Kind != "unknown" {
		t.Errorf("Kind = %q, want %q", c.Kind, "unknown")
	}
	if c.Offset() != 0 {
		t.Errorf("Offset = %d, want 0", c.Offset())
	}
}

func TestCursor_Equal_SameKind(t *testing.T) {
	a := ZeroCursor(CursorByteOffset)
	b := ZeroCursor(CursorByteOffset)
	if !a.Equal(b) {
		t.Error("identical zero cursors not equal")
	}
}

func TestCursor_Equal_DifferentKind(t *testing.T) {
	a := ZeroCursor(CursorByteOffset)
	b := ZeroCursor(CursorRowIDTime)
	if a.Equal(b) {
		t.Error("cursors of different kinds reported equal")
	}
}

func TestCursor_Equal_Nil(t *testing.T) {
	// Two nil cursors should be equal (both become &Cursor{}).
	a := (*Cursor)(nil)
	b := (*Cursor)(nil)
	if !a.Equal(b) {
		t.Error("two nil cursors not equal")
	}
	// A zero-kind cursor (nil becomes &Cursor{Kind:""}) is NOT equal
	// to a typed zero cursor (Kind="byte_offset").
	typed := ZeroCursor(CursorByteOffset)
	if typed.Equal(nil) {
		t.Error("byte_offset cursor equal to nil (empty kind)")
	}
}

func TestCursor_Equal_DifferentValue(t *testing.T) {
	a := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(10)}}
	b := &Cursor{Kind: CursorByteOffset, Value: map[string]any{"offset": int64(20)}}
	if a.Equal(b) {
		t.Error("cursors with different offsets reported equal")
	}
}

func TestCursor_Offset_Nil(t *testing.T) {
	var c *Cursor
	if c.Offset() != 0 {
		t.Errorf("nil cursor Offset = %d, want 0", c.Offset())
	}
}

func TestISOFromEpochMS(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  string
	}{
		{"nil", nil, ""},
		{"zero", int64(0), ""},
		{"negative", int64(-1), ""},
		{"seconds", int64(1700000000), "2023-11-14T22:13:20Z"},
		{"milliseconds", int64(1700000000000), "2023-11-14T22:13:20Z"},
		{"int", 1700000000, "2023-11-14T22:13:20Z"},
		{"float64", float64(1700000000), "2023-11-14T22:13:20Z"},
		{"rf3339 string", "2024-01-01T00:00:00Z", "2024-01-01T00:00:00Z"},
		{"invalid string", "not-a-date", ""},
		{"bad type", []int{1}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ISOFromEpochMS(tc.value)
			if got != tc.want {
				t.Errorf("ISOFromEpochMS(%v) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}
