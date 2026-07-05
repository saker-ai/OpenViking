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

func TestCohereEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v2/embed", r.URL.Path)
		require.Equal(t, "Bearer sk-co", r.Header.Get("Authorization"))
		var in cohereEmbedRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "embed-v4.0", in.Model)
		assert.Equal(t, "search_document", in.InputType)
		assert.Equal(t, []string{"hello", "world"}, in.Texts)
		// Server-side output_dimension is sent when Dim is set and supported.
		assert.Equal(t, 512, in.OutputDimension)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": map[string]any{
				"float": [][]float32{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}},
			},
		})
	}))
	defer srv.Close()
	c := NewCohere(srv.URL, "sk-co", "embed-v4.0", 512, srv.Client())
	got, err := c.Embed(context.Background(), []string{"hello", "world"}, "embed-v4.0")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, got[0], 1e-6)
}

func TestCohereEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer srv.Close()
	c := NewCohere(srv.URL, "bad", "embed-v4.0", 0, srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "embed-v4.0")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestCohereEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewCohere("", "k", "embed-v4.0", 0, nil)
	got, err := c.Embed(context.Background(), nil, "embed-v4.0")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestCohereEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewCohere("", "k", "", 0, nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestCohereEmbedClientSideTruncation(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in cohereEmbedRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		// embed-english-v3.0 does NOT support server-side output_dimension;
		// the client must omit it from the request and truncate the response.
		assert.Equal(t, 0, in.OutputDimension)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings": map[string]any{
				// Native 1024-dim vector returned; client truncates to 512.
				"float": [][]float32{buildF32(1024, 0.25)},
			},
		})
	}))
	defer srv.Close()
	c := NewCohere(srv.URL, "k", "embed-english-v3.0", 512, srv.Client())
	got, err := c.Embed(context.Background(), []string{"a"}, "embed-english-v3.0")
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Len(t, got[0], 512)
	// L2-normalized: sum of squares ~= 1.0.
	var sumSq float32
	for _, v := range got[0] {
		sumSq += v * v
	}
	assert.InDelta(t, 1.0, sumSq, 0.01)
}

func TestCohereDimensions(t *testing.T) {
	t.Parallel()
	c := NewCohere("", "k", "", 0, nil)
	assert.Equal(t, 1536, c.Dimensions("embed-v4.0"))
	assert.Equal(t, 1024, c.Dimensions("embed-multilingual-v3.0"))
	assert.Equal(t, 384, c.Dimensions("embed-english-light-v3.0"))
	assert.Equal(t, 1024, c.Dimensions("unknown"))
	// Caller-pinned dim overrides the table.
	c2 := NewCohere("", "k", "", 256, nil)
	assert.Equal(t, 256, c2.Dimensions("embed-v4.0"))
}

// buildF32 returns an n-dim float32 slice where every slot equals v.
func buildF32(n int, v float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = v
	}
	return out
}
