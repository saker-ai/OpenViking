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

func TestDashscopeRerankSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/services/rerank/text-rerank/text-rerank", r.URL.Path)
		require.Equal(t, "Bearer sk-dash", r.Header.Get("Authorization"))
		var in dashscopeReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "gte-rerank-v2", in.Model)
		assert.Equal(t, "q", in.Input.Query)
		require.Len(t, in.Input.Documents, 2)
		assert.Equal(t, "alpha", in.Input.Documents[0])
		assert.Equal(t, "beta", in.Input.Documents[1])
		assert.Equal(t, 2, in.Parameters.TopN)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{
				"results": []map[string]any{
					{"index": 1, "relevance_score": 0.92},
					{"index": 0, "relevance_score": 0.45},
				},
			},
			"request_id": "req-1",
		})
	}))
	defer srv.Close()
	c := NewDashscope(srv.URL+"/api/v1", "sk-dash", "gte-rerank-v2", srv.Client())
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	got, err := c.Rerank(context.Background(), "q", docs, 2)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, "b", got[0].ID)
	assert.InDelta(t, 0.92, got[0].Score, 1e-6)
	assert.Equal(t, "a", got[1].ID)
	assert.InDelta(t, 0.45, got[1].Score, 1e-6)
}

func TestDashscopeRerankMissingModel(t *testing.T) {
	t.Parallel()
	c := NewDashscope("", "k", "", nil)
	_, err := c.Rerank(context.Background(), "q", []Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestDashscopeRerankMissingQuery(t *testing.T) {
	t.Parallel()
	c := NewDashscope("", "k", "gte-rerank-v2", nil)
	_, err := c.Rerank(context.Background(), "", []Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
}

func TestDashscopeRerankEmpty(t *testing.T) {
	t.Parallel()
	c := NewDashscope("", "k", "m", nil)
	got, err := c.Rerank(context.Background(), "q", nil, 1)
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestDashscopeRerankDefaultsBaseURL(t *testing.T) {
	t.Parallel()
	c := NewDashscope("", "k", "m", nil)
	assert.Equal(t, DefaultDashscopeBaseURL, c.BaseURL)
}

func TestDashscopeRerankHTTPError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom"))
	}))
	defer srv.Close()
	c := NewDashscope(srv.URL, "k", "gte-rerank-v2", srv.Client())
	_, err := c.Rerank(context.Background(), "q", []Document{{ID: "x", Content: "y"}}, 1)
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrRerankFailed))
}

func TestDashscopeRerankTopNClampedToDocsLen(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in dashscopeReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		// topN > docs len must be clamped to docs len.
		assert.Equal(t, 2, in.Parameters.TopN)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"output": map[string]any{
				"results": []map[string]any{
					{"index": 0, "relevance_score": 0.9},
					{"index": 1, "relevance_score": 0.2},
				},
			},
		})
	}))
	defer srv.Close()
	c := NewDashscope(srv.URL, "k", "gte-rerank-v2", srv.Client())
	docs := []Document{
		{ID: "a", Content: "alpha"},
		{ID: "b", Content: "beta"},
	}
	got, err := c.Rerank(context.Background(), "q", docs, 99)
	require.NoError(t, err)
	require.Len(t, got, 2)
}
