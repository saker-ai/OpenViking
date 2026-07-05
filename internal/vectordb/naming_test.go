package vectordb

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

func TestCollectionName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		prefix, account, kind, want string
	}{
		{"ov_", "acme", "file", "ov_acme__file"},
		{"", "acme", "file", "acme__file"},
		{"ov_", "a.b-c_1", "kind.x", "ov_a.b-c_1__kind.x"},
	}
	for _, tc := range cases {
		got, err := CollectionName(tc.prefix, tc.account, tc.kind)
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}

func TestCollectionNameRejectsBadInput(t *testing.T) {
	t.Parallel()
	cases := [][]string{
		{"ov_", "", "file"},     // empty account
		{"ov_", "acct", ""},     // empty kind
		{"ov_", "a__b", "file"}, // __ in account
		{"ov_", "acct", "k__d"}, // __ in kind
		{"ov_", "a b", "file"},  // space in account
		{"ov_", "acct", "a/b"},  // slash in kind
	}
	for _, tc := range cases {
		_, err := CollectionName(tc[0], tc[1], tc[2])
		require.Error(t, err, "input=%v", tc)
		var ae *domain.AppError
		require.ErrorAs(t, err, &ae)
		assert.Equal(t, domain.CodeValidationFailed, ae.Code)
	}
}

func TestSplitCollectionName(t *testing.T) {
	t.Parallel()
	account, kind, err := SplitCollectionName("ov_acme__file", "ov_")
	require.NoError(t, err)
	assert.Equal(t, "acme", account)
	assert.Equal(t, "file", kind)

	// Wrong prefix.
	_, _, err = SplitCollectionName("other_acme__file", "ov_")
	require.Error(t, err)

	// Missing separator.
	_, _, err = SplitCollectionName("ov_acmefile", "ov_")
	require.Error(t, err)
}

func TestFilterMatches(t *testing.T) {
	t.Parallel()
	row := Vector{
		ID:        "id1",
		Embedding: []float32{1, 0, 0},
		Metadata: map[string]any{
			"account": "acme",
			"kind":    "file",
			"uri":     "viking://acme/file/foo.txt",
			"lang":    "go",
			"count":   int64(3),
		},
	}
	cases := []struct {
		name   string
		filter Filter
		want   bool
	}{
		{"empty", Filter{}, true},
		{"account match", Filter{Account: "acme"}, true},
		{"account miss", Filter{Account: "other"}, false},
		{"kind match", Filter{Kind: "file"}, true},
		{"uri prefix match", Filter{URIPrefix: "viking://acme/file/"}, true},
		{"uri prefix miss", Filter{URIPrefix: "viking://other/"}, false},
		{"metadata match", Filter{Metadata: map[string]any{"lang": "go"}}, true},
		{"metadata miss", Filter{Metadata: map[string]any{"lang": "py"}}, false},
		{"metadata int64", Filter{Metadata: map[string]any{"count": int64(3)}}, true},
		{"combined", Filter{Account: "acme", Kind: "file", URIPrefix: "viking://acme"}, true},
		{"combined miss", Filter{Account: "acme", Kind: "dir"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.filter.Matches(row))
		})
	}
}
