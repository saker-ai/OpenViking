package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("OV_CONFIG_PATH", "")
	cfg, err := Load("")
	require.NoError(t, err)
	require.NotNil(t, cfg)

	assert.Equal(t, "0.0.0.0", cfg.Server.Host)
	assert.Equal(t, 8080, cfg.Server.Port)
	assert.Equal(t, int64(1<<30), cfg.Server.MaxUploadSize)
	assert.Equal(t, "qdrant", cfg.VectorDB.Backend)
	assert.Equal(t, "ov_", cfg.VectorDB.CollectionPrefix)
	assert.Equal(t, "memory", cfg.RAGFS.Cache.Provider)
	assert.Equal(t, "argon2id", cfg.Auth.APIKey.HashAlgo)
	assert.Equal(t, "memory", cfg.OTEL.Exporter)
	assert.Equal(t, "openviking-server", cfg.OTEL.ServiceName)
	assert.Equal(t, 1.0, cfg.OTEL.SampleRate)
}

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	confPath := filepath.Join(dir, "ov.conf")
	const yaml = `server:
  host: 127.0.0.1
  port: 9090
vectordb:
  backend: local
  local:
    path: /tmp/ovdb
`
	require.NoError(t, os.WriteFile(confPath, []byte(yaml), 0o600))
	t.Setenv("OV_CONFIG_PATH", "")

	cfg, err := Load(confPath)
	require.NoError(t, err)

	assert.Equal(t, "127.0.0.1", cfg.Server.Host)
	assert.Equal(t, 9090, cfg.Server.Port)
	assert.Equal(t, "local", cfg.VectorDB.Backend)
	assert.Equal(t, "/tmp/ovdb", cfg.VectorDB.Local.Path)
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("OV_CONFIG_PATH", "")
	t.Setenv("OV_SERVER_PORT", "7777")
	t.Setenv("OV_VECTORDB_BACKEND", "qdrant")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 7777, cfg.Server.Port)
	assert.Equal(t, "qdrant", cfg.VectorDB.Backend)
}

func TestServerAddr(t *testing.T) {
	s := ServerConfig{Host: "1.2.3.4", Port: 80}
	assert.Equal(t, "1.2.3.4:80", s.Addr())
}

// TestApplyDashscopeEnvLocalProviderSkipped verifies that DASHSCOPE_* env
// vars do NOT pollute the local embedder/rerank config — filling APIBase on
// a local embedder triggers NewLocal's OpenAI sidecar path which then fails
// with "model is required" because local config has no model.
func TestApplyDashscopeEnvLocalProviderSkipped(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "sk-test-123")
	t.Setenv("DASHSCOPE_BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1")
	t.Setenv("SAKER_MODEL_BASE_URL", "https://dashscope.aliyuncs.com/apps/anthropic")
	t.Setenv("VOLCENGINE_ACCESS_KEY", "AKTEST")
	t.Setenv("VOLCENGINE_SECRET_KEY", "SKTEST")
	t.Setenv("VOLCENGINE_REGION", "cn-beijing")

	cfg := &Config{
		VLM:      VLMConfig{Provider: ""},
		Embedder: EmbedderConfig{Provider: "local", Dim: 64},
		Rerank:   RerankConfig{Provider: "cohere", TopN: 5},
		VectorDB: VectorDBConfig{Backend: "memory"},
	}
	applyDashscopeEnv(cfg)

	// local embedder must NOT get APIKey/APIBase — would trigger sidecar path.
	assert.Empty(t, cfg.Embedder.APIKey, "local embedder must not inherit DASHSCOPE_API_KEY")
	assert.Empty(t, cfg.Embedder.APIBase, "local embedder must not inherit DASHSCOPE_BASE_URL")
	// empty VLM provider must NOT get env vars.
	assert.Empty(t, cfg.VLM.APIKey)
	assert.Empty(t, cfg.VLM.APIBase)
	// memory vectordb must NOT get volcengine keys.
	assert.Empty(t, cfg.VectorDB.VikingDB.AccessKey)
	assert.Empty(t, cfg.VectorDB.Volcengine.APIKey)
	// rerank provider=cohere is a real provider — env SHOULD be applied.
	assert.Equal(t, "sk-test-123", cfg.Rerank.APIKey)
	assert.Equal(t, "https://dashscope.aliyuncs.com/compatible-mode/v1", cfg.Rerank.APIBase)
}

// TestApplyDashscopeEnvRealProvidersApplied verifies env vars populate
// fields when provider is a real remote backend.
func TestApplyDashscopeEnvRealProvidersApplied(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "sk-real-456")
	t.Setenv("DASHSCOPE_BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1")
	t.Setenv("SAKER_MODEL_BASE_URL", "https://dashscope.aliyuncs.com/apps/anthropic")
	t.Setenv("VOLCENGINE_ACCESS_KEY", "AKREAL")
	t.Setenv("VOLCENGINE_SECRET_KEY", "SKREAL")
	t.Setenv("VOLCENGINE_REGION", "cn-beijing-2")

	cfg := &Config{
		VLM:      VLMConfig{Provider: "dashscope"},
		Embedder: EmbedderConfig{Provider: "dashscope"},
		Rerank:   RerankConfig{Provider: "dashscope", TopN: 5},
		VectorDB: VectorDBConfig{Backend: "vikingdb"},
	}
	applyDashscopeEnv(cfg)

	assert.Equal(t, "sk-real-456", cfg.VLM.APIKey)
	// SAKER_MODEL_BASE_URL takes precedence over DASHSCOPE_BASE_URL for VLM.
	assert.Equal(t, "https://dashscope.aliyuncs.com/apps/anthropic", cfg.VLM.APIBase)
	assert.Equal(t, "sk-real-456", cfg.Embedder.APIKey)
	assert.Equal(t, "https://dashscope.aliyuncs.com/compatible-mode/v1", cfg.Embedder.APIBase)
	assert.Equal(t, "sk-real-456", cfg.Rerank.APIKey)
	assert.Equal(t, "AKREAL", cfg.VectorDB.VikingDB.AccessKey)
	assert.Equal(t, "SKREAL", cfg.VectorDB.VikingDB.SecretKey)
	assert.Equal(t, "cn-beijing-2", cfg.VectorDB.VikingDB.Region)
}

// TestApplyDashscopeEnvConfigFileWins verifies on-disk values are NOT
// overwritten by env vars.
func TestApplyDashscopeEnvConfigFileWins(t *testing.T) {
	t.Setenv("DASHSCOPE_API_KEY", "sk-env-should-lose")

	cfg := &Config{
		VLM:      VLMConfig{Provider: "dashscope", APIKey: "sk-from-file", APIBase: "https://from-file"},
		Embedder: EmbedderConfig{Provider: "dashscope", APIKey: "sk-embed-file"},
		VectorDB: VectorDBConfig{Backend: "vikingdb", VikingDB: VikingDBConfig{AccessKey: "AK-FILE"}},
	}
	applyDashscopeEnv(cfg)

	assert.Equal(t, "sk-from-file", cfg.VLM.APIKey)
	assert.Equal(t, "https://from-file", cfg.VLM.APIBase)
	assert.Equal(t, "sk-embed-file", cfg.Embedder.APIKey)
	assert.Equal(t, "AK-FILE", cfg.VectorDB.VikingDB.AccessKey)
}
