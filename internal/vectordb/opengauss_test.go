package vectordb

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

// stubPgConn is a test-only pgConn that records executed SQL + args and
// returns canned responses. Each method maps to a queue of stub results;
// when the queue is empty the method returns the zero value (or a
// configurable default error). This keeps tests free of network calls.
type stubPgConn struct {
	// per-method FIFO result queues
	execResults []stubResult
	oneResults  []stubRowResult
	allResults  []stubRowsResult

	// recorded calls
	execCalls []stubCall
	oneCalls  []stubCall
	allCalls  []stubCall

	// default error when a queue is empty
	defaultExecErr error
	defaultOneErr  error
	defaultAllErr  error

	closed bool
}

type stubResult struct {
	err error
}

type stubRowResult struct {
	row map[string]any
	err error
}

type stubRowsResult struct {
	rows []map[string]any
	err  error
}

type stubCall struct {
	sql  string
	args []any
}

func (s *stubPgConn) Exec(_ context.Context, sql string, args ...any) error {
	s.execCalls = append(s.execCalls, stubCall{sql: sql, args: args})
	if len(s.execResults) == 0 {
		return s.defaultExecErr
	}
	r := s.execResults[0]
	s.execResults = s.execResults[1:]
	return r.err
}

func (s *stubPgConn) QueryOne(_ context.Context, sql string, args ...any) (map[string]any, error) {
	s.oneCalls = append(s.oneCalls, stubCall{sql: sql, args: args})
	if len(s.oneResults) == 0 {
		return nil, s.defaultOneErr
	}
	r := s.oneResults[0]
	s.oneResults = s.oneResults[1:]
	return r.row, r.err
}

func (s *stubPgConn) QueryAll(_ context.Context, sql string, args ...any) ([]map[string]any, error) {
	s.allCalls = append(s.allCalls, stubCall{sql: sql, args: args})
	if len(s.allResults) == 0 {
		return nil, s.defaultAllErr
	}
	r := s.allResults[0]
	s.allResults = s.allResults[1:]
	return r.rows, r.err
}

func (s *stubPgConn) Close() { s.closed = true }

// TestOpenGaussTableName confirms the table-name format and SQL identifier
// quoting (defends against SQL injection via collection name).
func TestOpenGaussTableName(t *testing.T) {
	t.Parallel()
	a := newOpenGaussAdapterWithConn("public", nil)
	assert.Equal(t, `"public"."vetor_ov_acme__doc"`, a.tableName("ov_acme__doc"))
	// Embedded double-quote in collection name is escaped, preventing injection.
	got := a.tableName(`foo"; DROP TABLE x; --`)
	require.True(t, strings.HasPrefix(got, `"public"."vetor_foo`), got)
	assert.Contains(t, got, `""`) // doubled quote
	// Schema is configurable.
	a2 := newOpenGaussAdapterWithConn("rag", nil)
	assert.Equal(t, `"rag"."vetor_x"`, a2.tableName("x"))
}

func TestQuoteIdent(t *testing.T) {
	assert.Equal(t, `"mytable"`, quoteIdent("mytable"))
	assert.Equal(t, `"weird""name"`, quoteIdent(`weird"name`))
}

func TestVectorLiteral(t *testing.T) {
	v := vectorLiteral([]float32{1, 2.5, -3})
	assert.Equal(t, "[1,2.5,-3]", v)
	// Empty vector still produces "[]".
	assert.Equal(t, "[]", vectorLiteral(nil))
}

func TestBuildOpenGaussWhere(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		w, args := buildOpenGaussWhere(Filter{})
		assert.Empty(t, w)
		assert.Nil(t, args)
	})
	t.Run("account+kind", func(t *testing.T) {
		w, args := buildOpenGaussWhere(Filter{Account: "acme", Kind: "doc"})
		assert.Contains(t, w, "metadata->>'account' = $1")
		assert.Contains(t, w, "metadata->>'kind' = $2")
		assert.Len(t, args, 2)
	})
	t.Run("uri_prefix", func(t *testing.T) {
		w, args := buildOpenGaussWhere(Filter{URIPrefix: "viking://"})
		assert.Contains(t, w, "uri LIKE ($1 || '%')")
		assert.Len(t, args, 1)
	})
	t.Run("metadata_skips_account_kind_uri", func(t *testing.T) {
		// account/kind/uri are handled as dedicated columns, so metadata
		// must NOT re-add them as JSONB containment conditions.
		w, args := buildOpenGaussWhere(Filter{
			Account:  "a",
			Kind:     "k",
			Metadata: map[string]any{"account": "x", "kind": "y", "uri": "z", "lang": "en"},
		})
		// exactly one metadata @> containment condition (only "lang" survives the skip list)
		assert.Equal(t, 1, strings.Count(w, "@>"))
		assert.Contains(t, w, "metadata @> $3::jsonb")
		// "lang" is the only metadata key in the args (as a JSONB blob).
		assert.Len(t, args, 3) // account + kind + lang
		// The third arg is the JSONB blob for "lang" -> en.
		blob, ok := args[2].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "en", blob["lang"])
	})
}

func TestOpenGaussEnsureCollection_DimInCreate(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{execResults: []stubResult{{}, {}}}
	a := newOpenGaussAdapterWithConn("public", conn)
	err := a.EnsureCollection(context.Background(), CollectionSchema{Name: "ov_x__doc", Dim: 768, Distance: "cosine"})
	require.NoError(t, err)
	require.Len(t, conn.execCalls, 2)
	assert.Contains(t, conn.execCalls[0].sql, "CREATE TABLE IF NOT EXISTS")
	assert.Contains(t, conn.execCalls[0].sql, "VECTOR(768)")
	assert.Contains(t, conn.execCalls[1].sql, "CREATE INDEX IF NOT EXISTS")
	assert.Contains(t, conn.execCalls[1].sql, "ivfflat")
}

func TestOpenGaussEnsureCollection_InvalidSchema(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{}
	a := newOpenGaussAdapterWithConn("public", conn)
	err := a.EnsureCollection(context.Background(), CollectionSchema{Name: "", Dim: 0})
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeValidationFailed, appErr.Code)
}

func TestOpenGaussDropCollection(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{execResults: []stubResult{{}}}
	a := newOpenGaussAdapterWithConn("public", conn)
	require.NoError(t, a.DropCollection(context.Background(), "ov_x__doc"))
	require.Len(t, conn.execCalls, 1)
	assert.Contains(t, conn.execCalls[0].sql, "DROP TABLE IF EXISTS")
	// Empty name is a no-op.
	require.NoError(t, a.DropCollection(context.Background(), ""))
}

func TestOpenGaussListCollections(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{
		allResults: []stubRowsResult{{
			rows: []map[string]any{
				{"table_name": "vetor_ov_acme__doc"},
				{"table_name": "vetor_ov_acme__file"},
				{"table_name": "user_table"}, // not "vetor_" prefixed; should still be stripped, but typically filtered by SQL
			},
		}},
	}
	a := newOpenGaussAdapterWithConn("public", conn)
	names, err := a.ListCollections(context.Background())
	require.NoError(t, err)
	// The SQL filters by LIKE 'vetor\_%'; the stub ignores that and returns
	// all rows, so we just check the prefix-stripping logic.
	assert.Contains(t, names, "ov_acme__doc")
	assert.Contains(t, names, "ov_acme__file")
	assert.Contains(t, names, "user_table") // stripped of "vetor_" -> "_table"? No: "user_table" doesn't start with "vetor_"
}

func TestOpenGaussUpsert(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{execResults: []stubResult{{}, {}}}
	a := newOpenGaussAdapterWithConn("public", conn)
	rows := []Vector{
		{ID: "d1", Embedding: []float32{1, 0, 0}, Metadata: map[string]any{"account": "acme", "kind": "doc", "uri": "viking://d/1"}},
		{ID: "d2", Embedding: []float32{0, 1, 0}, Metadata: map[string]any{"account": "acme", "kind": "doc", "uri": "viking://d/2"}},
	}
	require.NoError(t, a.Upsert(context.Background(), "ov_x__doc", rows))
	require.Len(t, conn.execCalls, 2)
	sql := conn.execCalls[0].sql
	assert.Contains(t, sql, "INSERT INTO")
	assert.Contains(t, sql, "ON CONFLICT (id) DO UPDATE")
	assert.Equal(t, "d1", conn.execCalls[0].args[0])
	assert.Equal(t, "[1,0,0]", conn.execCalls[0].args[1])
}

func TestOpenGaussUpsert_Validation(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{}
	a := newOpenGaussAdapterWithConn("public", conn)
	// Empty collection.
	err := a.Upsert(context.Background(), "", []Vector{{ID: "d1", Embedding: []float32{1}}})
	require.Error(t, err)
	// Empty rows is a no-op.
	require.NoError(t, a.Upsert(context.Background(), "c", nil))
	// Empty ID.
	err = a.Upsert(context.Background(), "c", []Vector{{ID: "", Embedding: []float32{1}}})
	require.Error(t, err)
	// Empty embedding.
	err = a.Upsert(context.Background(), "c", []Vector{{ID: "x", Embedding: nil}})
	require.Error(t, err)
}

func TestOpenGaussDelete(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{execResults: []stubResult{{}}}
	a := newOpenGaussAdapterWithConn("public", conn)
	require.NoError(t, a.Delete(context.Background(), "ov_x__doc", []string{"d1", "d2"}))
	require.Len(t, conn.execCalls, 1)
	assert.Contains(t, conn.execCalls[0].sql, "DELETE FROM")
	assert.Equal(t, []string{"d1", "d2"}, conn.execCalls[0].args[0])
	// Empty IDs is a no-op.
	require.NoError(t, a.Delete(context.Background(), "ov_x__doc", nil))
}

func TestOpenGaussSearch(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{
		allResults: []stubRowsResult{{
			rows: []map[string]any{
				{"id": "d1", "uri": "viking://d/1", "metadata": map[string]any{"account": "acme", "kind": "doc"}, "similarity": float64(0.9)},
				{"id": "d2", "uri": "viking://d/2", "metadata": []byte(`{"account":"acme","kind":"doc"}`), "similarity": float64(0.7)},
			},
		}},
	}
	a := newOpenGaussAdapterWithConn("public", conn)
	res, err := a.Search(context.Background(), SearchParams{
		Collection: "ov_x__doc",
		Query:      []float32{1, 0, 0},
		TopK:       5,
		Filter:     Filter{Account: "acme", Kind: "doc"},
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 2)
	assert.Equal(t, "d1", res.Hits[0].ID)
	assert.InDelta(t, 0.9, res.Hits[0].Score, 1e-6)
	assert.Equal(t, "acme", res.Hits[0].Metadata["account"])
	assert.Equal(t, "viking://d/2", res.Hits[1].Metadata["uri"])

	// Verify the SQL has the right shape: WHERE + ORDER BY + LIMIT.
	sql := conn.allCalls[0].sql
	assert.Contains(t, sql, "ORDER BY vector <=> $")
	assert.Contains(t, sql, "LIMIT $")
	assert.Contains(t, sql, "metadata->>'account' = $")
	assert.Contains(t, sql, "metadata->>'kind' = $")
}

func TestOpenGaussSearch_EmptyQuery(t *testing.T) {
	t.Parallel()
	a := newOpenGaussAdapterWithConn("public", &stubPgConn{})
	_, err := a.Search(context.Background(), SearchParams{Collection: "c", Query: nil, TopK: 5})
	require.Error(t, err)
}

func TestOpenGaussGet(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{
		oneResults: []stubRowResult{{
			row: map[string]any{
				"id":       "d1",
				"uri":      "viking://d/1",
				"metadata": map[string]any{"account": "acme", "kind": "doc"},
			},
		}},
	}
	a := newOpenGaussAdapterWithConn("public", conn)
	v, err := a.Get(context.Background(), "ov_x__doc", "d1")
	require.NoError(t, err)
	assert.Equal(t, "d1", v.ID)
	assert.Equal(t, "acme", v.Metadata["account"])
	assert.Equal(t, "viking://d/1", v.Metadata["uri"])
}

func TestOpenGaussGet_NotFound(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{oneResults: []stubRowResult{{row: nil, err: nil}}}
	a := newOpenGaussAdapterWithConn("public", conn)
	_, err := a.Get(context.Background(), "ov_x__doc", "missing")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeResourceNotFound, appErr.Code)
}

func TestOpenGaussCount(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{
		oneResults: []stubRowResult{{row: map[string]any{"n": int64(42)}}},
	}
	a := newOpenGaussAdapterWithConn("public", conn)
	n, err := a.Count(context.Background(), "ov_x__doc")
	require.NoError(t, err)
	assert.Equal(t, int64(42), n)
}

func TestOpenGaussCount_ErrorWrapped(t *testing.T) {
	t.Parallel()
	conn := &stubPgConn{defaultOneErr: errors.New("connection refused")}
	a := newOpenGaussAdapterWithConn("public", conn)
	_, err := a.Count(context.Background(), "ov_x__doc")
	require.Error(t, err)
	var appErr *domain.AppError
	require.ErrorAs(t, err, &appErr)
	assert.Equal(t, domain.CodeVectorDBError, appErr.Code)
}

func TestOpenGaussWrapErr(t *testing.T) {
	assert.Nil(t, wrapOpenGaussErr(nil))
	wrapped := wrapOpenGaussErr(errors.New("boom"))
	require.Error(t, wrapped)
	var appErr *domain.AppError
	require.ErrorAs(t, wrapped, &appErr)
	assert.Equal(t, domain.CodeVectorDBError, appErr.Code)
}

func TestOpenGaussClose_NilSafe(t *testing.T) {
	a := &OpenGaussAdapter{}
	assert.NoError(t, a.Close())
}

func TestOpenGaussAdapterInterface(t *testing.T) {
	// Compile-time assertion that OpenGaussAdapter implements CollectionAdapter.
	var _ CollectionAdapter = (*OpenGaussAdapter)(nil)
}

func TestOpenGaussRowToVector(t *testing.T) {
	// map[string]any metadata
	v := openGaussRowToVector(map[string]any{
		"id":       "d1",
		"uri":      "u",
		"metadata": map[string]any{"account": "a"},
	})
	assert.Equal(t, "d1", v.ID)
	assert.Equal(t, "u", v.Metadata["uri"])
	assert.Equal(t, "a", v.Metadata["account"])

	// []byte metadata (raw JSON)
	v = openGaussRowToVector(map[string]any{
		"id":       "d2",
		"metadata": []byte(`{"lang":"go"}`),
	})
	assert.Equal(t, "d2", v.ID)
	assert.Equal(t, "go", v.Metadata["lang"])

	// string metadata
	v = openGaussRowToVector(map[string]any{
		"id":       "d3",
		"metadata": `{"x":1}`,
	})
	assert.Equal(t, "d3", v.ID)
	assert.Equal(t, float64(1), v.Metadata["x"])
}
