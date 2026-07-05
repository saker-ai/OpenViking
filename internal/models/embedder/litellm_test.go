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

func TestLiteLLMEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/embeddings", r.URL.Path)
		require.Equal(t, "Bearer litellm-key", r.Header.Get("Authorization"))
		var in litellmRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "openai/text-embedding-3-small", in.Model)
		assert.Equal(t, []string{"hello", "world"}, in.Input)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3, 0.4, 0.5}},
				{"embedding": []float32{0.6, 0.7, 0.8, 0.9, 1.0}},
			},
		})
	}))
	defer srv.Close()
	c := NewLiteLLM(srv.URL, "litellm-key", "openai/text-embedding-3-small", 3, srv.Client())
	got, err := c.Embed(context.Background(), []string{"hello", "world"}, "openai/text-embedding-3-small")
	require.NoError(t, err)
	require.Len(t, got, 2)
	// LiteLLM client-side truncation: 5-dim vectors truncated to 3.
	require.Len(t, got[0], 3)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, got[0], 1e-6)
}

func TestLiteLLMEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"proxy down"}`))
	}))
	defer srv.Close()
	c := NewLiteLLM(srv.URL, "k", "openai/text-embedding-3-small", 4, srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "openai/text-embedding-3-small")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestLiteLLMEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewLiteLLM("", "k", "openai/text-embedding-3-small", 4, nil)
	got, err := c.Embed(context.Background(), nil, "openai/text-embedding-3-small")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLiteLLMEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewLiteLLM("", "k", "", 4, nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestLiteLLMDimensions(t *testing.T) {
	t.Parallel()
	c := NewLiteLLM("", "k", "openai/text-embedding-3-small", 1536, nil)
	assert.Equal(t, 1536, c.Dimensions("anything"))
}
