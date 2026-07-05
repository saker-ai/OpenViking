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

func TestGeminiEmbedSingleSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1beta/models/gemini-embedding-2-preview:embedContent", r.URL.Path)
		require.Equal(t, "gemkey", r.Header.Get("x-goog-api-key"))
		var in geminiEmbedRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Len(t, in.Content.Parts, 1)
		assert.Equal(t, "hello", in.Content.Parts[0].Text)
		assert.Equal(t, "RETRIEVAL_DOCUMENT", in.TaskType)
		assert.Equal(t, 768, in.OutputDimensionality)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embedding": map[string]any{"values": []float32{0.1, 0.2, 0.3}},
		})
	}))
	defer srv.Close()
	c := NewGemini(srv.URL, "gemkey", "gemini-embedding-2-preview", 768, srv.Client())
	c.TaskType = "retrieval_document"
	got, err := c.Embed(context.Background(), []string{"hello"}, "gemini-embedding-2-preview")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, got[0], 1e-6)
}

func TestGeminiEmbedBatchSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1beta/models/gemini-embedding-001:batchEmbedContent", r.URL.Path)
		var in geminiBatchRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		require.Len(t, in.Requests, 2)
		assert.Equal(t, "alpha", in.Requests[0].Content.Parts[0].Text)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": []map[string]any{
				{"values": []float32{0.1}},
				{"values": []float32{0.2}},
			},
		})
	}))
	defer srv.Close()
	c := NewGemini(srv.URL, "k", "gemini-embedding-001", 0, srv.Client())
	got, err := c.Embed(context.Background(), []string{"alpha", "beta"}, "gemini-embedding-001")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDelta(t, 0.1, got[0][0], 1e-6)
	assert.InDelta(t, 0.2, got[1][0], 1e-6)
}

func TestGeminiEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"permission denied"}`))
	}))
	defer srv.Close()
	c := NewGemini(srv.URL, "k", "gemini-embedding-2-preview", 0, srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "gemini-embedding-2-preview")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestGeminiEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewGemini("", "k", "gemini-embedding-2-preview", 0, nil)
	got, err := c.Embed(context.Background(), nil, "gemini-embedding-2-preview")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestGeminiEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewGemini("", "k", "", 0, nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestGeminiDimensions(t *testing.T) {
	t.Parallel()
	c := NewGemini("", "k", "", 0, nil)
	assert.Equal(t, 3072, c.Dimensions("gemini-embedding-2-preview"))
	assert.Equal(t, 3072, c.Dimensions("gemini-embedding-001"))
	assert.Equal(t, 768, c.Dimensions("text-embedding-004"))
	// Prefix rule: text-embedding-* -> 768.
	assert.Equal(t, 768, c.Dimensions("text-embedding-005"))
	// Fallback: 3072 for unknown gemini-embedding-* models.
	assert.Equal(t, 3072, c.Dimensions("gemini-embedding-99"))
	// Caller-pinned dim overrides the table.
	c2 := NewGemini("", "k", "", 1024, nil)
	assert.Equal(t, 1024, c2.Dimensions("gemini-embedding-2-preview"))
}
