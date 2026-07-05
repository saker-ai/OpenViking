package rerank

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/domain"
)

func TestCohereRerankSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/rerank", r.URL.Path)
		require.Equal(t, "Bearer sk-co", r.Header.Get("Authorization"))
		var in cohereReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "what is rag", in.Query)
		assert.Equal(t, "rerank-multilingual-v3.0", in.Model)
		require.Len(t, in.Documents, 3)
		assert.Equal(t, "doc a", in.Documents[0])
		assert.Equal(t, 2, in.TopN)

		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 1, "relevance_score": 0.95},
				{"index": 0, "relevance_score": 0.42},
			},
		})
	}))
	defer srv.Close()
	c := NewCohere(srv.URL+"/v1", "sk-co", "rerank-multilingual-v3.0", srv.Client())
	docs := []Document{
		{ID: "a", Content: "doc a"},
		{ID: "b", Content: "doc b"},
		{ID: "c", Content: "doc c"},
	}
	got, err := c.Rerank(context.Background(), "what is rag", docs, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].ID)
	assert.InDelta(t, 0.95, got[0].Score, 1e-6)
	assert.Equal(t, "a", got[1].ID)
	assert.InDelta(t, 0.42, got[1].Score, 1e-6)
}

func TestCohereRerankEmpty(t *testing.T) {
	t.Parallel()
	c := NewCohere("", "k", "m", nil)
	got, err := c.Rerank(context.Background(), "q", nil, 5)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestCohereRerankMissingQuery(t *testing.T) {
	t.Parallel()
	c := NewCohere("", "k", "m", nil)
	_, err := c.Rerank(context.Background(), "", []Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestCohereRerankTopNClamped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"index": 0, "relevance_score": 0.9}},
		})
	}))
	defer srv.Close()
	c := NewCohere(srv.URL, "k", "m", srv.Client())
	docs := []Document{{ID: "a", Content: "x"}}
	got, err := c.Rerank(context.Background(), "q", docs, 100)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestCohereRerankErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewCohere(srv.URL, "bad", "m", srv.Client())
	_, err := c.Rerank(context.Background(), "q", []Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrRerankFailed))
}

func TestVolcengineRerankSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/rerank", r.URL.Path)
		require.Equal(t, "Bearer sk-ark", r.Header.Get("Authorization"))
		var in volcReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "doubao-rerank-pro", in.Model)
		require.Len(t, in.Documents, 2)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 1, "relevance_score": 0.88},
				{"index": 0, "relevance_score": 0.31},
			},
		})
	}))
	defer srv.Close()
	c := NewVolcengine(srv.URL+"/api/v3", "sk-ark", "doubao-rerank-pro", srv.Client())
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	got, err := c.Rerank(context.Background(), "q", docs, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].ID)
	assert.InDelta(t, 0.88, got[0].Score, 1e-6)
}

func TestVolcengineRerankMissingModel(t *testing.T) {
	t.Parallel()
	c := NewVolcengine("", "k", "", nil)
	_, err := c.Rerank(context.Background(), "q", []Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestVolcengineRerankEmpty(t *testing.T) {
	t.Parallel()
	c := NewVolcengine("", "k", "m", nil)
	got, err := c.Rerank(context.Background(), "q", nil, 1)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestVolcengineDefaultsBaseURL(t *testing.T) {
	t.Parallel()
	c := NewVolcengine("", "k", "m", nil)
	assert.Equal(t, DefaultVolcengineBaseURL, c.BaseURL)
}

func TestLocalRerankCosine(t *testing.T) {
	t.Parallel()
	r := NewLocal()
	docs := []Document{
		{ID: "rag", Content: "retrieval augmented generation is a technique"},
		{ID: "llm", Content: "large language models are powerful"},
		{ID: "irrelevant", Content: "the weather is sunny today"},
	}
	got, err := r.Rerank(context.Background(),
		"what is retrieval augmented generation", docs, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// The "rag" doc should outrank the "irrelevant" doc.
	assert.Equal(t, "rag", got[0].ID)
	assert.Greater(t, got[0].Score, got[1].Score)
}

func TestLocalRerankTopNAll(t *testing.T) {
	t.Parallel()
	r := NewLocal()
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
		{ID: "c", Content: "gamma"},
	}
	got, err := r.Rerank(context.Background(), "alpha", docs, 0)
	require.NoError(t, err)
	assert.Len(t, got, 3)
	// Sorted descending: "alpha" should be first.
	assert.Equal(t, "a", got[0].ID)
}

func TestLocalRerankEmpty(t *testing.T) {
	t.Parallel()
	r := NewLocal()
	got, err := r.Rerank(context.Background(), "q", nil, 5)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLocalRerankMissingQuery(t *testing.T) {
	t.Parallel()
	r := NewLocal()
	_, err := r.Rerank(context.Background(), "",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestLocalRerankUsesCrossEncoder(t *testing.T) {
	t.Parallel()
	called := false
	ce := &stubReranker{fn: func(_ context.Context, _ string, _ []Document, _ int) ([]Document, error) {
		called = true
		return []Document{{ID: "ce"}}, nil
	}}
	r := &LocalReranker{CrossEncoder: ce}
	got, err := r.Rerank(context.Background(), "q",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.NoError(t, err)
	require.True(t, called)
	require.Len(t, got, 1)
	assert.Equal(t, "ce", got[0].ID)
}

func TestCosineZeroVectors(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 0.0, cosine(nil, nil))
	assert.Equal(t, 0.0, cosine([]float32{0, 0}, []float32{1, 1}))
}

func TestCosineIdentical(t *testing.T) {
	t.Parallel()
	v := []float32{1, 0, 0}
	assert.InDelta(t, 1.0, cosine(v, v), 1e-6)
}

func TestSqrtF32(t *testing.T) {
	t.Parallel()
	assert.Equal(t, float32(0), sqrtF32(0))
	assert.InDelta(t, 2.0, sqrtF32(4.0), 0.01)
}

// stubReranker is a test-only Reranker that delegates to a closure.
type stubReranker struct {
	fn func(ctx context.Context, query string, docs []Document, topN int) ([]Document, error)
}

func (s *stubReranker) Rerank(ctx context.Context, q string, d []Document, n int) ([]Document, error) {
	return s.fn(ctx, q, d, n)
}

var _ Reranker = (*stubReranker)(nil)
