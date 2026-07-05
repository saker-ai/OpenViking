package eval

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDatasetFromReader_AcceptsPythonShape(t *testing.T) {
	t.Parallel()
	in := `{"question": "q1", "answer": "a1", "files": ["f1", "f2"]}
{"question": "q2", "answer": "a2", "category": "docs"}
`
	logOut := &bytes.Buffer{}
	cases, err := LoadDatasetFromReader(strings.NewReader(in), logOut)
	require.NoError(t, err)
	require.Len(t, cases, 2)
	assert.Equal(t, "q1", cases[0].Query)
	assert.Equal(t, "a1", cases[0].GroundTruth)
	assert.Equal(t, "f1,f2", cases[0].Meta["files"])
	assert.Equal(t, "q2", cases[1].Query)
	assert.Equal(t, "docs", cases[1].Meta["category"])
	assert.Empty(t, logOut.String(), "no warnings expected")
}

func TestLoadDatasetFromReader_AcceptsGoShape(t *testing.T) {
	t.Parallel()
	in := `{"query": "q1", "ground_truth": "gt1", "id": "x1"}
`
	cases, err := LoadDatasetFromReader(strings.NewReader(in), nil)
	require.NoError(t, err)
	require.Len(t, cases, 1)
	assert.Equal(t, "x1", cases[0].ID)
	assert.Equal(t, "q1", cases[0].Query)
	assert.Equal(t, "gt1", cases[0].GroundTruth)
}

func TestLoadDatasetFromReader_SkipsBadLines(t *testing.T) {
	t.Parallel()
	in := `{"question": "good"}

not json at all
{"question": ""}
{"question": "also good", "answer": "a2"}
`
	logOut := &bytes.Buffer{}
	cases, err := LoadDatasetFromReader(strings.NewReader(in), logOut)
	require.NoError(t, err)
	require.Len(t, cases, 2)
	assert.Equal(t, "good", cases[0].Query)
	assert.Equal(t, "also good", cases[1].Query)
	// Two warnings: bad JSON line + missing 'question'.
	assert.Contains(t, logOut.String(), "invalid JSON")
	assert.Contains(t, logOut.String(), "missing 'question' field")
}

func TestLoadDatasetFromReader_EmptyInput(t *testing.T) {
	t.Parallel()
	cases, err := LoadDatasetFromReader(strings.NewReader(""), nil)
	require.NoError(t, err)
	assert.Empty(t, cases)
}

func TestLoadDatasetFromReader_BlankLinesIgnored(t *testing.T) {
	t.Parallel()
	in := "\n\n{\"question\": \"q1\"}\n\n\n"
	cases, err := LoadDatasetFromReader(strings.NewReader(in), nil)
	require.NoError(t, err)
	require.Len(t, cases, 1)
	assert.Equal(t, "q1", cases[0].Query)
}

func TestLoadDataset_MissingFile(t *testing.T) {
	t.Parallel()
	_, err := LoadDataset("/nonexistent/path/file.jsonl", nil)
	require.Error(t, err)
}
