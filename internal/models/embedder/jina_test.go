package embedder

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

func TestJinaEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/embeddings", r.URL.Path)
		require.Equal(t, "Bearer jina_xxx", r.Header.Get("Authorization"))
		var in jinaRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "jina-embeddings-v5-text-small", in.Model)
		assert.Equal(t, []string{"hello", "world"}, in.Input)
		assert.Equal(t, 512, in.Dimensions)
		assert.Equal(t, "retrieval.passage", in.Task)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}},
				{"embedding": []float32{0.4, 0.5, 0.6}},
			},
		})
	}))
	defer srv.Close()
	c := NewJina(srv.URL, "jina_xxx", "jina-embeddings-v5-text-small", 512, srv.Client())
	c.Task = "retrieval.passage"
	got, err := c.Embed(context.Background(), []string{"hello", "world"}, "jina-embeddings-v5-text-small")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, got[0], 1e-6)
}

func TestJinaEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad key"}`))
	}))
	defer srv.Close()
	c := NewJina(srv.URL, "bad", "jina-embeddings-v5-text-small", 0, srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "jina-embeddings-v5-text-small")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestJinaEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewJina("", "k", "jina-embeddings-v5-text-small", 0, nil)
	got, err := c.Embed(context.Background(), nil, "jina-embeddings-v5-text-small")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestJinaEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewJina("", "k", "", 0, nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestJinaDimensions(t *testing.T) {
	t.Parallel()
	c := NewJina("", "k", "", 0, nil)
	assert.Equal(t, 1024, c.Dimensions("jina-embeddings-v5-text-small"))
	assert.Equal(t, 768, c.Dimensions("jina-embeddings-v5-text-nano"))
	assert.Equal(t, 1024, c.Dimensions("jina-code-embeddings-1.5b"))
	assert.Equal(t, 1024, c.Dimensions("unknown"))
	c2 := NewJina("", "k", "", 256, nil)
	assert.Equal(t, 256, c2.Dimensions("jina-embeddings-v5-text-small"))
}
