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

func TestVikingDBEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/vikingdb/embedding", r.URL.Path)
		// The SDK signs every request with Volcengine V4 HMAC; assert that
		// an Authorization header is present rather than any test-only marker.
		require.NotEmpty(t, r.Header.Get("Authorization"))
		require.NotEmpty(t, r.Header.Get("X-Date"))
		var in vikingDBRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "bge-large-zh", in.DenseModel.Name)
		assert.Equal(t, 1024, in.DenseModel.Dim)
		require.Len(t, in.Data, 2)
		assert.Equal(t, "hello", in.Data[0].Text)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": map[string]any{
				"data": []map[string]any{
					{"dense_embedding": []float32{0.1, 0.2, 0.3}},
					{"dense_embedding": []float32{0.4, 0.5, 0.6}},
				},
			},
		})
	}))
	defer srv.Close()
	c := NewVikingDB(srv.URL, "ak", "sk", "bge-large-zh", "", 1024, nil).WithHTTPClient(srv.Client())
	got, err := c.Embed(context.Background(), []string{"hello", "world"}, "bge-large-zh")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, got[0], 1e-6)
}

func TestVikingDBEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"ak/sk invalid"}`))
	}))
	defer srv.Close()
	c := NewVikingDB(srv.URL, "bad-ak", "bad-sk", "bge-large-zh", "", 1024, nil).WithHTTPClient(srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "bge-large-zh")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestVikingDBEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewVikingDB("", "ak", "sk", "bge-large-zh", "", 1024, nil)
	got, err := c.Embed(context.Background(), nil, "bge-large-zh")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestVikingDBEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewVikingDB("", "ak", "sk", "", "", 1024, nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestVikingDBDimensions(t *testing.T) {
	t.Parallel()
	c := NewVikingDB("", "ak", "sk", "bge-large-zh", "", 1024, nil)
	assert.Equal(t, 1024, c.Dimensions("bge-large-zh"))
	c2 := NewVikingDB("", "ak", "sk", "bge-large-zh", "", 0, nil)
	assert.Equal(t, 2048, c2.Dimensions("bge-large-zh"))
}
