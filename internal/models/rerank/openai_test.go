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

func TestOpenAIRerankSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// apiBase is the FULL url (path included), so r.URL.Path is the
		// server's root path.
		require.Equal(t, "/", r.URL.Path)
		require.Equal(t, "Bearer dashscope-key", r.Header.Get("Authorization"))
		var in openaiReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "qwen3-rerank", in.Model)
		assert.Equal(t, "what is rag", in.Query)
		require.Len(t, in.Documents, 2)
		assert.Equal(t, "alpha", in.Documents[0])
		assert.Equal(t, "beta", in.Documents[1])
		assert.Equal(t, 2, in.TopN)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 1, "relevance_score": 0.88},
				{"index": 0, "relevance_score": 0.32},
			},
		})
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL, "dashscope-key", "qwen3-rerank", srv.Client())
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	got, err := c.Rerank(context.Background(), "what is rag", docs, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].ID)
	assert.InDelta(t, 0.88, got[0].Score, 1e-6)
	assert.Equal(t, "a", got[1].ID)
	assert.InDelta(t, 0.32, got[1].Score, 1e-6)
}

func TestOpenAIRerankErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL, "bad", "qwen3-rerank", srv.Client())
	_, err := c.Rerank(context.Background(), "q",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrRerankFailed))
}

func TestOpenAIRerankMissingQuery(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "k", "m", nil)
	_, err := c.Rerank(context.Background(), "",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestOpenAIRerankMissingModel(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "k", "", nil)
	_, err := c.Rerank(context.Background(), "q",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestOpenAIRerankEmpty(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "k", "m", nil)
	got, err := c.Rerank(context.Background(), "q", nil, 1)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestOpenAIRerankExtraHeaders(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "tenant-abc", r.Header.Get("X-Tenant-Id"))
		// Authorization must NOT be overridable via ExtraHeaders.
		assert.Equal(t, "Bearer k", r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{"index": 0, "relevance_score": 0.9}},
		})
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL, "k", "m", srv.Client())
	c.ExtraHeaders = map[string]string{
		"X-Tenant-Id":   "tenant-abc",
		"Authorization": "Bearer override-attempt", // must be ignored
	}
	got, err := c.Rerank(context.Background(), "q",
		[]Document{{ID: "x", Content: "y"}}, 1)
	require.NoError(t, err)
	require.Len(t, got, 1)
}

func TestOpenAIRerankTopNClamped(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in openaiReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, 2, in.TopN)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{
				{"index": 0, "relevance_score": 0.9},
				{"index": 1, "relevance_score": 0.2},
			},
		})
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL, "k", "m", srv.Client())
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	got, err := c.Rerank(context.Background(), "q", docs, 99)
	require.NoError(t, err)
	require.Len(t, got, 2)
}
