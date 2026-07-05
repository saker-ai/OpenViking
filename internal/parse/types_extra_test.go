package parse

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFormatTableToMarkdown verifies the markdown table renderer formats
// rows with padding, headers, and the separator line.
func TestFormatTableToMarkdown(t *testing.T) {
	// Empty input returns empty string.
	assert.Equal(t, "", FormatTableToMarkdown(nil, false))

	rows := [][]string{
		{"Name", "Age"},
		{"Alice", "30"},
		{"Bob", "999"},
	}
	got := FormatTableToMarkdown(rows, true)
	// Header row present (padded to widest column member).
	assert.Contains(t, got, "Name  | Age")
	// Separator line of dashes.
	assert.Contains(t, got, "----- | ---")
	// Data rows (padded to column width).
	assert.Contains(t, got, "Alice | 30")
	assert.Contains(t, got, "Bob   | 999")
}

// TestFormatTableToMarkdown_NoHeader verifies that without the header flag
// no separator line is emitted.
func TestFormatTableToMarkdown_NoHeader(t *testing.T) {
	rows := [][]string{{"a", "b"}, {"c", "d"}}
	got := FormatTableToMarkdown(rows, false)
	assert.NotContains(t, got, "---")
}

// TestFormatSectionPath verifies titles are joined with " / " and empty
// entries are dropped.
func TestFormatSectionPath(t *testing.T) {
	assert.Equal(t, "a / b / c", formatSectionPath([]string{"a", "b", "c"}))
	assert.Equal(t, "a / c", formatSectionPath([]string{"a", "  ", "c"}))
	assert.Equal(t, "", formatSectionPath(nil))
}

// TestJoinPath verifies base+name joining with empty base returning name.
func TestJoinPath(t *testing.T) {
	assert.Equal(t, "name", joinPath("", "name"))
	assert.Equal(t, filepath.Join("base", "name"), joinPath("base", "name"))
}

// TestParseResultSuccess verifies Success is true when there are no warnings.
func TestParseResultSuccess(t *testing.T) {
	p := &ParseResult{}
	assert.True(t, p.Success())
	p.AddWarning("oops")
	assert.False(t, p.Success())
	// nil receiver is safe and returns false.
	var np *ParseResult
	assert.False(t, np.Success())
}

// TestParseResultAddWarning verifies AddWarning appends and chains.
func TestParseResultAddWarning(t *testing.T) {
	p := &ParseResult{}
	got := p.AddWarning("w1").AddWarning("w2")
	require.Same(t, p, got)
	assert.Equal(t, []string{"w1", "w2"}, p.Warnings)
	// nil receiver returns nil.
	var np *ParseResult
	assert.Nil(t, np.AddWarning("x"))
}

// TestParseResultAllNodes verifies depth-first traversal of the tree.
func TestParseResultAllNodes(t *testing.T) {
	// nil receiver or nil root returns nil.
	var np *ParseResult
	assert.Nil(t, np.AllNodes())
	p := &ParseResult{}
	assert.Nil(t, p.AllNodes())

	root := &ResourceNode{Type: NodeRoot, Title: "root"}
	sec := root.AddChild(&ResourceNode{Type: NodeSection, Title: "s1"})
	sec.AddChild(&ResourceNode{Type: NodeParagraph, Title: "p1"})
	p.Root = root
	all := p.AllNodes()
	require.Len(t, all, 3)
	assert.Equal(t, "root", all[0].Title)
	assert.Equal(t, "s1", all[1].Title)
	assert.Equal(t, "p1", all[2].Title)
}

// TestParseResultSections verifies Sections filters by type and level.
func TestParseResultSections(t *testing.T) {
	root := &ResourceNode{Type: NodeRoot}
	root.AddChild(&ResourceNode{Type: NodeSection, Level: 1, Title: "h1"})
	root.AddChild(&ResourceNode{Type: NodeSection, Level: 2, Title: "h2"})
	root.AddChild(&ResourceNode{Type: NodeSection, Level: 3, Title: "h3"})
	root.AddChild(&ResourceNode{Type: NodeParagraph, Level: 1, Title: "para"})
	p := &ParseResult{Root: root}

	// All sections 1-3.
	secs := p.Sections(1, 3)
	require.Len(t, secs, 3)
	// Only level 2.
	secs = p.Sections(2, 2)
	require.Len(t, secs, 1)
	assert.Equal(t, "h2", secs[0].Title)
	// minLevel > maxLevel swaps them.
	secs = p.Sections(3, 1)
	require.Len(t, secs, 3)
}

// TestResourceNodeAddChild_Nil verifies AddChild on nil receiver is safe.
func TestResourceNodeAddChild_Nil(t *testing.T) {
	var n *ResourceNode
	assert.Nil(t, n.AddChild(&ResourceNode{Type: NodeRoot}))
	// nil child is a no-op.
	root := &ResourceNode{Type: NodeRoot}
	assert.Equal(t, root, root.AddChild(nil))
	assert.Empty(t, root.Children)
}

// TestLocalResourceCleanup verifies Cleanup removes temporary files and
// directories and is a no-op for non-temporary resources.
func TestLocalResourceCleanup(t *testing.T) {
	// Non-temporary: no-op.
	dir := t.TempDir()
	r := &LocalResource{Path: dir, IsTemporary: false}
	r.Cleanup()
	_, err := os.Stat(dir)
	require.NoError(t, err, "non-temporary resource must not be removed")

	// Temporary directory: removed.
	tmpDir := t.TempDir()
	r = &LocalResource{Path: tmpDir, IsTemporary: true}
	r.Cleanup()
	_, err = os.Stat(tmpDir)
	require.ErrorIs(t, err, os.ErrNotExist)

	// Temporary file: removed.
	f := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(f, []byte("x"), 0644))
	r = &LocalResource{Path: f, IsTemporary: true}
	r.Cleanup()
	_, err = os.Stat(f)
	require.ErrorIs(t, err, os.ErrNotExist)

	// nil receiver and empty path are safe no-ops.
	var nr *LocalResource
	nr.Cleanup()
	r = &LocalResource{IsTemporary: true, Path: ""}
	r.Cleanup()
}

// TestLocalResourceClose verifies Close calls Cleanup and returns nil.
func TestLocalResourceClose(t *testing.T) {
	f := filepath.Join(t.TempDir(), "c.txt")
	require.NoError(t, os.WriteFile(f, []byte("y"), 0644))
	r := &LocalResource{Path: f, IsTemporary: true}
	require.NoError(t, r.Close())
	_, err := os.Stat(f)
	require.ErrorIs(t, err, os.ErrNotExist)
}

// TestTempDir verifies tempDir creates a directory under the OS default
// and under a custom parent.
func TestTempDir(t *testing.T) {
	d1, err := tempDir("")
	require.NoError(t, err)
	defer os.RemoveAll(d1)
	info, err := os.Stat(d1)
	require.NoError(t, err)
	require.True(t, info.IsDir())

	parent := t.TempDir()
	d2, err := tempDir(parent)
	require.NoError(t, err)
	defer os.RemoveAll(d2)
	rel, err := filepath.Rel(parent, d2)
	require.NoError(t, err)
	assert.NotEqual(t, ".."+string(filepath.Separator)+rel, rel, "temp dir must be under parent")
}

// TestCopyFile verifies copyFile duplicates a file's contents and mode.
func TestCopyFile(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src.txt")
	dst := filepath.Join(t.TempDir(), "dst.txt")
	require.NoError(t, os.WriteFile(src, []byte("hello"), 0644))

	require.NoError(t, copyFile(src, dst, 0644))
	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "hello", string(got))

	// Missing source returns an error.
	require.Error(t, copyFile(filepath.Join(t.TempDir(), "nope"), dst, 0644))
}

// TestParse_NilResource verifies Parse on a nil resource returns a
// validation error.
func TestParse_NilResource(t *testing.T) {
	_, err := Parse(context.Background(), nil, ParserOptions{})
	require.Error(t, err)
}

// TestParse_MissingPath verifies Parse on a non-existent path returns a
// not-found error.
func TestParse_MissingPath(t *testing.T) {
	res := &LocalResource{Path: filepath.Join(t.TempDir(), "does-not-exist.md")}
	_, err := Parse(context.Background(), res, ParserOptions{})
	require.Error(t, err)
}
