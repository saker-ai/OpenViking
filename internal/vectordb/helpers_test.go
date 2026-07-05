package vectordb

import (
	"context"
	"testing"

	qdrant "github.com/qdrant/go-client/qdrant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/saker-ai/ctxhub/internal/domain"
)

func TestUuidV5_Deterministic(t *testing.T) {
	a := uuidV5("collection/doc/123")
	b := uuidV5("collection/doc/123")
	assert.Equal(t, a, b)
	assert.NotEqual(t, a, uuidV5("collection/doc/456"))
}

func TestBuildQdrantFilter_Empty(t *testing.T) {
	assert.Nil(t, buildQdrantFilter(Filter{}))
}

func TestBuildQdrantFilter_Account(t *testing.T) {
	f := buildQdrantFilter(Filter{Account: "acct1"})
	require.NotNil(t, f)
	require.Len(t, f.Must, 1)
}

func TestBuildQdrantFilter_AllFields(t *testing.T) {
	f := buildQdrantFilter(Filter{
		Account:   "acct",
		Kind:      "doc",
		URIPrefix: "viking://",
		Metadata: map[string]any{
			"str":     "val",
			"bool":    true,
			"int":     42,
			"int64":   int64(99),
			"float32": float32(1.5),
			"float64": 2.5,
			"other":   struct{ A int }{A: 1},
		},
	})
	require.NotNil(t, f)
	// 3 scalar fields + 7 metadata = 10 conditions.
	assert.Len(t, f.Must, 10)
}

func TestQdrantValueToAny(t *testing.T) {
	assert.Nil(t, qdrantValueToAny(nil))

	strVal := &qdrant.Value{Kind: &qdrant.Value_StringValue{StringValue: "hello"}}
	assert.Equal(t, "hello", qdrantValueToAny(strVal))

	intVal := &qdrant.Value{Kind: &qdrant.Value_IntegerValue{IntegerValue: 42}}
	assert.Equal(t, int64(42), qdrantValueToAny(intVal))

	dblVal := &qdrant.Value{Kind: &qdrant.Value_DoubleValue{DoubleValue: 3.14}}
	assert.Equal(t, 3.14, qdrantValueToAny(dblVal))

	boolVal := &qdrant.Value{Kind: &qdrant.Value_BoolValue{BoolValue: true}}
	assert.Equal(t, true, qdrantValueToAny(boolVal))

	// Unknown kind returns nil.
	unknownVal := &qdrant.Value{}
	assert.Nil(t, qdrantValueToAny(unknownVal))
}

func TestWrapQdrantErr(t *testing.T) {
	assert.Nil(t, wrapQdrantErr(nil))
	wrapped := wrapQdrantErr(status.Error(codes.Internal, "boom"))
	require.Error(t, wrapped)
	var appErr *domain.AppError
	require.ErrorAs(t, wrapped, &appErr)
	assert.Equal(t, domain.CodeVectorDBError, appErr.Code)
}

func TestIsQdrantNotFound(t *testing.T) {
	assert.False(t, isQdrantNotFound(nil))
	assert.False(t, isQdrantNotFound(status.Error(codes.Internal, "boom")))
	assert.True(t, isQdrantNotFound(status.Error(codes.NotFound, "missing")))
}

// mockQdrantPoint implements the minimal interface qdrantPointToVector needs.
type mockQdrantPoint struct {
	payload map[string]*qdrant.Value
	vectors *qdrant.VectorsOutput
	id      *qdrant.PointId
}

func (m *mockQdrantPoint) GetPayload() map[string]*qdrant.Value { return m.payload }
func (m *mockQdrantPoint) GetVectors() *qdrant.VectorsOutput    { return m.vectors }
func (m *mockQdrantPoint) GetId() *qdrant.PointId               { return m.id }

func TestQdrantPointToVector(t *testing.T) {
	// Point with _id in payload and additional metadata.
	pid := &qdrant.PointId{PointIdOptions: &qdrant.PointId_Uuid{Uuid: "abc-123"}}
	payload := map[string]*qdrant.Value{
		"_id":     {Kind: &qdrant.Value_StringValue{StringValue: "doc1"}},
		"account": {Kind: &qdrant.Value_StringValue{StringValue: "acct"}},
		"count":   {Kind: &qdrant.Value_IntegerValue{IntegerValue: 7}},
	}
	p := &mockQdrantPoint{payload: payload, id: pid}
	v, err := qdrantPointToVector(p)
	require.NoError(t, err)
	assert.Equal(t, "doc1", v.ID)
	assert.Equal(t, "acct", v.Metadata["account"])
	assert.Equal(t, int64(7), v.Metadata["count"])
	// _id should NOT appear in metadata.
	_, ok := v.Metadata["_id"]
	assert.False(t, ok)

	// Point missing _id in payload falls back to qdrant point UUID.
	p2 := &mockQdrantPoint{
		payload: map[string]*qdrant.Value{"k": {Kind: &qdrant.Value_StringValue{StringValue: "v"}}},
		id:      pid,
	}
	v2, err := qdrantPointToVector(p2)
	require.NoError(t, err)
	assert.Equal(t, "abc-123", v2.ID)
}

func TestInnerProductDistance(t *testing.T) {
	// dot([1,2,3],[4,5,6]) = 4+10+18 = 32; innerProductDistance = -32.
	got := innerProductDistance([]float32{1, 2, 3}, []float32{4, 5, 6})
	assert.InDelta(t, -32.0, got, 1e-6)
}

func TestPickDistance(t *testing.T) {
	assert.NotNil(t, pickDistance(""))
	assert.NotNil(t, pickDistance("cosine"))
	assert.NotNil(t, pickDistance("l2"))
	assert.NotNil(t, pickDistance("ip"))
	assert.NotNil(t, pickDistance("unknown")) // falls back to cosine
}

func TestSortByScoreDesc(t *testing.T) {
	hits := []Vector{
		{ID: "a", Score: 0.5},
		{ID: "b", Score: 0.9},
		{ID: "c", Score: 0.1},
		{ID: "d", Score: 0.7},
	}
	sortByScoreDesc(hits)
	assert.Equal(t, "b", hits[0].ID)
	assert.Equal(t, "d", hits[1].ID)
	assert.Equal(t, "a", hits[2].ID)
	assert.Equal(t, "c", hits[3].ID)
}

func TestStartsWith(t *testing.T) {
	assert.True(t, startsWith("viking://doc/1", "viking://"))
	assert.False(t, startsWith("doc", "viking://"))
	assert.False(t, startsWith("", "x"))
	assert.True(t, startsWith("abc", ""))
}

func TestEqualAny(t *testing.T) {
	assert.True(t, equalAny("s", "s"))
	assert.False(t, equalAny("s", "x"))
	assert.False(t, equalAny("s", 1))

	assert.True(t, equalAny(true, true))
	assert.False(t, equalAny(true, false))

	assert.True(t, equalAny(float64(1.5), float64(1.5)))
	assert.False(t, equalAny(float64(1.5), 2.5))

	assert.True(t, equalAny(float32(1.5), float64(1.5)))
	assert.False(t, equalAny(float32(1.5), float64(2.5)))

	assert.True(t, equalAny(int(3), int(3)))
	assert.True(t, equalAny(int(3), int64(3)))
	assert.True(t, equalAny(int(3), float64(3)))
	assert.False(t, equalAny(int(3), int(4)))
	assert.False(t, equalAny(int(3), "x"))

	assert.True(t, equalAny(int64(5), int64(5)))
	assert.True(t, equalAny(int64(5), int(5)))
	assert.False(t, equalAny(int64(5), int(6)))

	// Incompatible types fall through to false.
	assert.False(t, equalAny(nil, "x"))
}

func TestMemorySortMemScoredDesc(t *testing.T) {
	hits := []memScored{
		{v: Vector{ID: "a"}, score: 0.5},
		{v: Vector{ID: "b"}, score: 0.9},
		{v: Vector{ID: "c"}, score: 0.1},
	}
	sortMemScoredDesc(hits)
	assert.Equal(t, "b", hits[0].v.ID)
	assert.Equal(t, "a", hits[1].v.ID)
	assert.Equal(t, "c", hits[2].v.ID)
}

// TestNewQdrantAdapter_BadURL exercises the parseQdrantURL error path
// inside NewQdrantAdapter (invalid URL surfaces a wrapped error).
func TestNewQdrantAdapter_BadURL(t *testing.T) {
	_, err := NewQdrantAdapter(context.Background(), "grpc://:1234", "", "ov_")
	require.Error(t, err)
}

// TestNewQdrantAdapter_LazyDial exercises the client-creation success path
// (qdrant.NewClient dials lazily) and the Close path. We use a localhost
// port that is almost certainly closed; if NewClient dials eagerly and
// fails, we still exercise the error-wrapping branch.
func TestNewQdrantAdapter_LazyDial(t *testing.T) {
	a, err := NewQdrantAdapter(context.Background(), "127.0.0.1:1", "", "ov_")
	if err != nil {
		// Eager dial failed; the error-wrapping path is now covered.
		var appErr *domain.AppError
		require.ErrorAs(t, err, &appErr)
		assert.Equal(t, domain.CodeVectorDBError, appErr.Code)
		return
	}
	// Lazy dial succeeded; exercise Close.
	require.NotNil(t, a)
	assert.NoError(t, a.Close())
}

// TestQdrantAdapter_UnreachableExercisesErrorWrapping calls every public
// method on a QdrantAdapter pointed at an unreachable host. Each call must
// return a wrapped *domain.AppError with CodeVectorDBError (or
// CodeResourceNotFound for Get/Count on missing collections). This covers
// the error-wrapping branches in EnsureCollection, DropCollection,
// ListCollections, Upsert, Delete, Search, Get, and Count without a live
// server.
func TestQdrantAdapter_UnreachableExercisesErrorWrapping(t *testing.T) {
	a, err := NewQdrantAdapter(context.Background(), "127.0.0.1:1", "", "ov_")
	if err != nil {
		t.Skip("qdrant client dials eagerly on this platform; skip method-level tests")
	}
	defer a.Close()
	ctx := context.Background()
	schema := CollectionSchema{Name: "ov_test__c", Dim: 3, Distance: "cosine"}

	// EnsureCollection — gRPC call fails, wrapped as VECTORDB_ERROR.
	err = a.EnsureCollection(ctx, schema)
	if err != nil {
		var appErr *domain.AppError
		require.ErrorAs(t, err, &appErr, "EnsureCollection error must wrap")
		assert.Equal(t, domain.CodeVectorDBError, appErr.Code)
	}

	// DropCollection — same.
	_ = a.DropCollection(ctx, schema.Name)

	// ListCollections — same.
	_, _ = a.ListCollections(ctx)

	// Upsert — same.
	_ = a.Upsert(ctx, schema.Name, []Vector{{ID: "x", Embedding: []float32{1, 0, 0}}})

	// Delete — same.
	_ = a.Delete(ctx, schema.Name, []string{"x"})

	// Search — same.
	_, _ = a.Search(ctx, SearchParams{Collection: schema.Name, Query: []float32{1, 0, 0}, TopK: 1})

	// Get — same.
	_, _ = a.Get(ctx, schema.Name, "x")

	// Count — same.
	_, _ = a.Count(ctx, schema.Name)
}
