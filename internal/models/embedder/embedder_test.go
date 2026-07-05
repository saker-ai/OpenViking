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

	"github.com/saker-ai/ctxhub/internal/config"
	"github.com/saker-ai/ctxhub/internal/domain"
)

func TestOpenAIEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/embeddings", r.URL.Path)
		require.Equal(t, "Bearer sk-test", r.Header.Get("Authorization"))
		var in map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "text-embedding-3-small", in["model"])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}},
				{"embedding": []float32{0.4, 0.5, 0.6}},
			},
		})
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL+"/v1", "sk-test", srv.Client())
	got, err := c.Embed(context.Background(), []string{"a", "b"}, "text-embedding-3-small")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, got[0])
	assert.Equal(t, []float32{0.4, 0.5, 0.6}, got[1])
}

func TestOpenAIEmbedBatches(t *testing.T) {
	t.Parallel()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{1.0}},
				{"embedding": []float32{2.0}},
			},
		})
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL, "k", srv.Client())
	c.BatchSize = 2 // split 4 texts into 2 calls
	got, err := c.Embed(context.Background(), []string{"a", "b", "c", "d"}, "m")
	require.NoError(t, err)
	require.Len(t, got, 4)
	assert.Equal(t, 2, calls)
}

func TestOpenAIEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "", nil)
	got, err := c.Embed(context.Background(), nil, "m")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestOpenAIEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "", nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestOpenAIEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad model"))
	}))
	defer srv.Close()
	c := NewOpenAI(srv.URL, "k", srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "m")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestOpenAIDimensions(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "", nil)
	assert.Equal(t, 1536, c.Dimensions("text-embedding-3-small"))
	assert.Equal(t, 3072, c.Dimensions("text-embedding-3-large"))
	assert.Equal(t, 1536, c.Dimensions("text-embedding-ada-002"))
	assert.Equal(t, 2048, c.Dimensions("doubao-embedding-large"))
	assert.Equal(t, 1024, c.Dimensions("bge-large-zh"))
	assert.Equal(t, 512, c.Dimensions("bge-small-en"))
	assert.Equal(t, 1536, c.Dimensions("unknown")) // fallback
}

func TestOpenAIDimensionsOverride(t *testing.T) {
	t.Parallel()
	c := NewOpenAI("", "", nil)
	c.DimByModel = map[string]int{"custom": 768}
	assert.Equal(t, 768, c.Dimensions("custom"))
	assert.Equal(t, 1536, c.Dimensions("text-embedding-3-small")) // fallthrough
}

func TestVolcengineDefaults(t *testing.T) {
	t.Parallel()
	c := NewVolcengine(config.EmbedderConfig{Model: "doubao-embedding-large", APIKey: "k"}, nil)
	assert.Equal(t, DefaultVolcengineBaseURL, c.BaseURL)
	assert.Equal(t, "k", c.APIKey)
	// No Dim override -> knownDefaultDim applies.
	assert.Equal(t, 2048, c.Dimensions("doubao-embedding-large"))
}

func TestVolcengineDimOverride(t *testing.T) {
	t.Parallel()
	c := NewVolcengine(config.EmbedderConfig{
		Model:  "doubao-embedding-large",
		APIKey: "k",
		Dim:    1024,
	}, nil)
	assert.Equal(t, 1024, c.Dimensions("doubao-embedding-large"))
}

// volcengineEmbedWireReq mirrors the wire shape that the arkruntime SDK
// emits for POST /api/v3/embeddings.
type volcengineEmbedWireReq struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

func TestVolcengineEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v3/embeddings", r.URL.Path)
		require.Equal(t, "Bearer sk-volc", r.Header.Get("Authorization"))
		var in volcengineEmbedWireReq
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "doubao-embedding-large", in.Model)
		require.Len(t, in.Input, 2)
		assert.Equal(t, "a", in.Input[0])
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"embedding": []float32{0.1, 0.2, 0.3}},
				{"embedding": []float32{0.4, 0.5, 0.6}},
			},
		})
	}))
	defer srv.Close()
	c := NewVolcengine(config.EmbedderConfig{
		Provider: "volcengine",
		Model:    "doubao-embedding-large",
		APIKey:   "sk-volc",
		APIBase:  srv.URL + "/api/v3",
	}, srv.Client())
	got, err := c.Embed(context.Background(), []string{"a", "b"}, "")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, got[0])
	assert.Equal(t, []float32{0.4, 0.5, 0.6}, got[1])
}

func TestVolcengineEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewVolcengine(config.EmbedderConfig{Provider: "volcengine", Model: "m", APIKey: "k"}, nil)
	got, err := c.Embed(context.Background(), nil, "m")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestVolcengineEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewVolcengine(config.EmbedderConfig{Provider: "volcengine", APIKey: "k"}, nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestVolcengineEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad model"))
	}))
	defer srv.Close()
	c := NewVolcengine(config.EmbedderConfig{
		Provider: "volcengine",
		Model:    "bad-model",
		APIKey:   "sk-volc",
		APIBase:  srv.URL + "/api/v3",
	}, srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "bad-model")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestLocalHashFallback(t *testing.T) {
	t.Parallel()
	c := NewLocal(config.EmbedderConfig{}, nil)
	got, err := c.Embed(context.Background(), []string{"hello", "world"}, "")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Len(t, got[0], 768) // default dim
	assert.Len(t, got[1], 768)
	// Deterministic: same text -> same vector.
	again, _ := c.Embed(context.Background(), []string{"hello"}, "")
	require.Len(t, again, 1)
	assert.Equal(t, got[0], again[0])
}

func TestLocalHashFallbackDifferentTextsDiffer(t *testing.T) {
	t.Parallel()
	c := NewLocal(config.EmbedderConfig{}, nil)
	got, _ := c.Embed(context.Background(), []string{"hello", "different"}, "")
	require.Len(t, got, 2)
	assert.NotEqual(t, got[0], got[1])
}

func TestLocalHashFallbackCustomDim(t *testing.T) {
	t.Parallel()
	c := NewLocal(config.EmbedderConfig{Dim: 4}, nil)
	got, _ := c.Embed(context.Background(), []string{"x"}, "")
	require.Len(t, got, 1)
	require.Len(t, got[0], 4)
	// Verify L2 normalization: sum of squares ~= 1.0.
	var sumSq float32
	for _, v := range got[0] {
		sumSq += v * v
	}
	assert.InDelta(t, 1.0, sumSq, 0.01)
}

func TestLocalHashFallbackEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewLocal(config.EmbedderConfig{}, nil)
	got, err := c.Embed(context.Background(), nil, "")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestLocalSidecarUsedWhenConfigured(t *testing.T) {
	t.Parallel()
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2}}},
		})
	}))
	defer srv.Close()
	c := NewLocal(config.EmbedderConfig{APIBase: srv.URL, APIKey: "k", Model: "nomic"}, srv.Client())
	got, err := c.Embed(context.Background(), []string{"a"}, "")
	require.NoError(t, err)
	require.True(t, called)
	require.Len(t, got, 1)
	assert.Equal(t, []float32{0.1, 0.2}, got[0])
}

func TestLocalSidecarFallsBackToHint(t *testing.T) {
	t.Parallel()
	c := NewLocal(config.EmbedderConfig{}, nil)
	_, err := c.MustHaveSidecar()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Install")
}

func TestLocalDimensionsHashFallback(t *testing.T) {
	t.Parallel()
	c := NewLocal(config.EmbedderConfig{Dim: 256}, nil)
	assert.Equal(t, 256, c.Dimensions("anything"))
}

func TestLocalDimensionsSidecar(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	c := NewLocal(config.EmbedderConfig{APIBase: srv.URL, APIKey: "k"}, srv.Client())
	assert.Equal(t, 1536, c.Dimensions("text-embedding-3-small"))
}

func TestLocalConfigFromBaseURL(t *testing.T) {
	t.Parallel()
	c := LocalConfigFromBaseURL("https://example.local/v1", 384)
	require.NotNil(t, c.Sidecar)
	assert.Equal(t, "https://example.local/v1", c.Sidecar.BaseURL)
}

func TestSqrtF32Zero(t *testing.T) {
	t.Parallel()
	assert.Equal(t, float32(0), sqrtF32(0))
	assert.Equal(t, float32(0), sqrtF32(-1))
	// Positive value: 4 -> 2
	assert.InDelta(t, 2.0, sqrtF32(4.0), 0.01)
}

func TestHashEmbedDeterministicAcrossDim(t *testing.T) {
	t.Parallel()
	a := hashEmbed("hello", 8)
	b := hashEmbed("hello", 8)
	assert.Equal(t, a, b)
	// Different dim -> different length, but the first 8 slots of the dim=16
	// vector should NOT necessarily match the dim=8 vector (different
	// slot index affects the hash).
	c := hashEmbed("hello", 16)
	require.Len(t, c, 16)
	assert.Len(t, a, 8)
}
