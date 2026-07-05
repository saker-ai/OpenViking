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

func TestMinimaxEmbedSuccess(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/embeddings", r.URL.Path)
		require.Equal(t, "Bearer minimax-key", r.Header.Get("Authorization"))
		// GroupId must be sent as a query parameter.
		assert.Equal(t, "grp-123", r.URL.Query().Get("GroupId"))
		var in minimaxRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&in))
		assert.Equal(t, "embo-01", in.Model)
		assert.Equal(t, "db", in.Type)
		assert.Equal(t, []string{"hello", "world"}, in.Texts)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"vectors": [][]float32{{0.1, 0.2, 0.3}, {0.4, 0.5, 0.6}},
			"base_resp": map[string]any{
				"status_code": 0,
				"status_msg":  "ok",
			},
		})
	}))
	defer srv.Close()
	c := NewMinimax(srv.URL+"/v1/embeddings", "minimax-key", "embo-01", 0, srv.Client())
	c.GroupID = "grp-123"
	got, err := c.Embed(context.Background(), []string{"hello", "world"}, "embo-01")
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.InDeltaSlice(t, []float32{0.1, 0.2, 0.3}, got[0], 1e-6)
}

func TestMinimaxEmbedErrorStatus(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"bad key"}`))
	}))
	defer srv.Close()
	c := NewMinimax(srv.URL, "bad", "embo-01", 0, srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "embo-01")
	require.Error(t, err)
	assert.True(t, errors.Is(err, domain.ErrEmbedFailed))
}

func TestMinimaxEmbedBusinessError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"vectors": [][]float32{},
			"base_resp": map[string]any{
				"status_code": 1001,
				"status_msg":  "rate limited",
			},
		})
	}))
	defer srv.Close()
	c := NewMinimax(srv.URL, "k", "embo-01", 0, srv.Client())
	_, err := c.Embed(context.Background(), []string{"a"}, "embo-01")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rate limited")
}

func TestMinimaxEmbedEmptyInput(t *testing.T) {
	t.Parallel()
	c := NewMinimax("", "k", "embo-01", 0, nil)
	got, err := c.Embed(context.Background(), nil, "embo-01")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestMinimaxEmbedMissingModel(t *testing.T) {
	t.Parallel()
	c := NewMinimax("", "k", "", 0, nil)
	_, err := c.Embed(context.Background(), []string{"a"}, "")
	require.Error(t, err)
}

func TestMinimaxDimensions(t *testing.T) {
	t.Parallel()
	c := NewMinimax("", "k", "", 0, nil)
	assert.Equal(t, 1536, c.Dimensions("embo-01"))
	c2 := NewMinimax("", "k", "", 768, nil)
	assert.Equal(t, 768, c2.Dimensions("embo-01"))
}
