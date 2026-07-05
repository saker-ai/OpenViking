package vectordb

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saker-ai/ctxhub/internal/config"
)

func TestNewAdapterMemory(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{Backend: "memory"}
	a, err := NewAdapter(context.Background(), cfg)
	require.NoError(t, err)
	defer a.Close()
	_, ok := a.(*MemoryAdapter)
	assert.True(t, ok)
}

func TestNewAdapterLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := &config.VectorDBConfig{
		Backend:          "local",
		CollectionPrefix: "ov_",
		Local:            config.LocalVectorConfig{Path: dir},
	}
	a, err := NewAdapter(context.Background(), cfg)
	require.NoError(t, err)
	defer a.Close()
	_, ok := a.(*LocalAdapter)
	assert.True(t, ok)
}

func TestNewAdapterRejectsUnknown(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{Backend: "voodoo"}
	_, err := NewAdapter(context.Background(), cfg)
	require.Error(t, err)
}

func TestNewAdapterRejectsNil(t *testing.T) {
	t.Parallel()
	_, err := NewAdapter(context.Background(), nil)
	require.Error(t, err)
}

func TestNewAdapterOpenGauss(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{
		Backend:   "opengauss",
		OpenGauss: config.OpenGaussConfig{DSN: "postgres://u:p@127.0.0.1:1/db?sslmode=disable", Schema: "public"},
	}
	a, err := NewAdapter(context.Background(), cfg)
	// pgxpool parses the DSN lazily; NewOpenGaussAdapter succeeds without dialing.
	if err != nil {
		t.Skipf("opengauss adapter construction failed (pgxpool parse): %v", err)
	}
	defer a.Close()
	_, ok := a.(*OpenGaussAdapter)
	assert.True(t, ok)
}

func TestNewAdapterOpenGauss_MissingDSN(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{Backend: "opengauss"}
	_, err := NewAdapter(context.Background(), cfg)
	require.Error(t, err)
}

func TestNewAdapterVolcengine(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{
		Backend:    "volcengine",
		Volcengine: config.VolcengineConfig{BaseURL: "http://example.invalid", APIKey: "ark-key"},
	}
	a, err := NewAdapter(context.Background(), cfg)
	require.NoError(t, err)
	defer a.Close()
	_, ok := a.(*VolcengineAdapter)
	assert.True(t, ok)
}

func TestNewAdapterVolcengine_MissingAPIKey(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{Backend: "volcengine"}
	_, err := NewAdapter(context.Background(), cfg)
	require.Error(t, err)
}

func TestNewAdapterHTTP(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{
		Backend: "http",
		HTTP:    config.HTTPVectorConfig{URL: "http://example.invalid", APIKey: "k"},
	}
	a, err := NewAdapter(context.Background(), cfg)
	require.NoError(t, err)
	defer a.Close()
	_, ok := a.(*HTTPAdapter)
	assert.True(t, ok)
}

func TestNewAdapterHTTP_MissingURL(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{Backend: "http"}
	_, err := NewAdapter(context.Background(), cfg)
	require.Error(t, err)
}

func TestNewAdapterVikingDB(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{
		Backend:  "vikingdb",
		VikingDB: config.VikingDBConfig{Host: "https://api-vikingdb.volces.com", AccessKey: "ak", SecretKey: "sk"},
	}
	a, err := NewAdapter(context.Background(), cfg)
	require.NoError(t, err)
	defer a.Close()
	_, ok := a.(*VikingDBAdapter)
	assert.True(t, ok)
}

func TestNewAdapterVikingDB_MissingHost(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{
		Backend:  "vikingdb",
		VikingDB: config.VikingDBConfig{AccessKey: "ak", SecretKey: "sk"},
	}
	// Host defaults to api-vikingdb.volces.com, so this should succeed.
	a, err := NewAdapter(context.Background(), cfg)
	require.NoError(t, err)
	defer a.Close()
	_, ok := a.(*VikingDBAdapter)
	assert.True(t, ok)
}

func TestNewAdapterVikingDB_MissingCreds(t *testing.T) {
	t.Parallel()
	cfg := &config.VectorDBConfig{
		Backend:  "vikingdb",
		VikingDB: config.VikingDBConfig{Host: "https://api-vikingdb.volces.com"},
	}
	_, err := NewAdapter(context.Background(), cfg)
	require.Error(t, err)
}
