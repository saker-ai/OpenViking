package embedder

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
)

func TestDashscopeDefaults(t *testing.T) {
	t.Parallel()
	c := NewDashscope(config.EmbedderConfig{Model: "text-embedding-v3", APIKey: "sk-dash"}, nil)
	assert.Equal(t, DefaultDashscopeBaseURL, c.BaseURL)
	assert.Equal(t, "sk-dash", c.APIKey)
	// No Dim override -> knownDefaultDim falls back to 1536 (unknown family).
	// Callers who need the documented v3 default (1024) set cfg.Dim explicitly.
	assert.Equal(t, 1536, c.Dimensions("text-embedding-v3"))
}

func TestDashscopeDimOverride(t *testing.T) {
	t.Parallel()
	c := NewDashscope(config.EmbedderConfig{
		Model: "text-embedding-v3",
		APIKey: "k",
		Dim:   1024,
	}, nil)
	assert.Equal(t, 1024, c.Dimensions("text-embedding-v3"))
}

func TestDashscopeHonorsAPIBase(t *testing.T) {
	t.Parallel()
	c := NewDashscope(config.EmbedderConfig{
		Model:   "text-embedding-v3",
		APIKey:  "k",
		APIBase: "https://custom.example/v1/",
	}, nil)
	assert.Equal(t, "https://custom.example/v1", c.BaseURL)
}

func TestDashscopeEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/compatible-mode/v1/embeddings", r.URL.Path)
		require.Equal(t, "Bearer sk-dash", r.Header.Get("Authorization"))
		var in map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "text-embedding-v3", in["model"])
		assert.Equal(t, []any{"hello", "world"}, in["input"])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []any{0.1, 0.2, 0.3}},
				{"embedding": []any{0.4, 0.5, 0.6}},
			},
		})
	}))
	defer srv.Close()
	c := NewDashscope(config.EmbedderConfig{
		Model:   "text-embedding-v3",
		APIKey:  "sk-dash",
		APIBase: srv.URL + "/compatible-mode/v1",
	}, srv.Client())
	got, err := c.Embed(context.Background(), []string{"hello", "world"}, "text-embedding-v3")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, got[0], 1e-6)
}

func TestDashscopeEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewDashscope(config.EmbedderConfig{Model: "text-embedding-v3", APIKey: "k"}, nil)
	got, err := c.Embed(context.Background(), nil, "text-embedding-v3")
	require.NoError(t, err)
	assert.Nil(t, got)
}
