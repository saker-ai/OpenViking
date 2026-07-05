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

func TestLiteLLMRerankSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/rerank", r.URL.Path)
		require.Equal(t, "Bearer litellm-key", r.Header.Get("Authorization"))
		var in litellmReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "cohere/rerank-multilingual-v3.0", in.Model)
		assert.Equal(t, "what is rag", in.Query)
		require.Len(t, in.Documents, 2)
		assert.Equal(t, "alpha", in.Documents[0].Text)
		assert.Equal(t, "beta", in.Documents[1].Text)
		assert.Equal(t, 2, in.TopN)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 1, "relevance_score": 0.92},
				{"index": 0, "relevance_score": 0.45},
			},
		})
	}))
	defer srv.Close()
	c := NewLiteLLM(srv.URL, "litellm-key", "cohere/rerank-multilingual-v3.0", srv.Client())
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	got, err := c.Rerank(context.Background(), "what is rag", docs, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].ID)
	assert.InDelta(t, 0.92, got[0].Score, 1e-6)
	assert.Equal(t, "a", got[1].ID)
	assert.InDelta(t, 0.45, got[1].Score, 1e-6)
}

func TestLiteLLMRerankErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"proxy down"}`))
	}))
	defer srv.Close()
	c := NewLiteLLM(srv.URL, "k", "cohere/rerank-multilingual-v3.0", srv.Client())
	_, err := c.Rerank(context.Background(), "q",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrRerankFailed))
}

func TestLiteLLMRerankMissingQuery(t *testing.T) {
	t.Parallel()
	c := NewLiteLLM("", "k", "m", nil)
	_, err := c.Rerank(context.Background(), "",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestLiteLLMRerankMissingModel(t *testing.T) {
	t.Parallel()
	c := NewLiteLLM("", "k", "", nil)
	_, err := c.Rerank(context.Background(), "q",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestLiteLLMRerankEmpty(t *testing.T) {
	t.Parallel()
	c := NewLiteLLM("", "k", "m", nil)
	got, err := c.Rerank(context.Background(), "q", nil, 1)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLiteLLMRerankTopNClamped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in litellmReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		// topN > docs len must be clamped to docs len.
		assert.Equal(t, 2, in.TopN)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.9},
				{"index": 1, "relevance_score": 0.2},
			},
		})
	}))
	defer srv.Close()
	c := NewLiteLLM(srv.URL, "k", "m", srv.Client())
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	got, err := c.Rerank(context.Background(), "q", docs, 99)
	require.NoError(t, err)
	require.Len(t, got, 2)
}
