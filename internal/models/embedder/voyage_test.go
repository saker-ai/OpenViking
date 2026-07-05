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

func TestVoyageEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/embeddings", r.URL.Path)
		require.Equal(t, "Bearer voyage-key", r.Header.Get("Authorization"))
		var in voyageRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "voyage-3-large", in.Model)
		assert.Equal(t, []string{"hello", "world"}, in.Input)
		// Voyage uses output_dimension (NOT OpenAI's dimensions).
		assert.Equal(t, 512, in.OutputDimension)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}},
				{"embedding": []float32{0.4, 0.5, 0.6}},
			},
		})
	}))
	defer srv.Close()
	c := NewVoyage(srv.URL, "voyage-key", "voyage-3-large", 512, srv.Client())
	got, err := c.Embed(context.Background(), []string{"hello", "world"}, "voyage-3-large")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, got[0], 1e-6)
}

func TestVoyageEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad key"}`))
	}))
	defer srv.Close()
	c := NewVoyage(srv.URL, "bad", "voyage-3-large", 0, srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "voyage-3-large")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestVoyageEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewVoyage("", "k", "voyage-3-large", 0, nil)
	got, err := c.Embed(context.Background(), nil, "voyage-3-large")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestVoyageEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewVoyage("", "k", "", 0, nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestVoyageDimensions(t *testing.T) {
	t.Parallel()
	c := NewVoyage("", "k", "", 0, nil)
	assert.Equal(t, 1024, c.Dimensions("voyage-3"))
	assert.Equal(t, 1024, c.Dimensions("voyage-3-large"))
	assert.Equal(t, 1024, c.Dimensions("voyage-code-3"))
	assert.Equal(t, 1024, c.Dimensions("voyage-finance-2"))
	assert.Equal(t, 1024, c.Dimensions("unknown"))
	c2 := NewVoyage("", "k", "", 256, nil)
	assert.Equal(t, 256, c2.Dimensions("voyage-3"))
}
