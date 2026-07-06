// Package config loads OpenViking server/runtime configuration from
// defaults, files, environment variables, and command-line flags.
//
// The load order (later overrides earlier) follows the design doc:
//
//  1. Built-in defaults (see setDefaults)
//  2. /etc/openviking/ov.conf
//  3. $HOME/.config/openviking/ov.conf
//  4. $OV_CONFIG_PATH
//  5. Environment variables (prefix OV_, dots -> underscores)
//  6. Command-line flags (passed *pflag.FlagSet)
//
// All field names and YAML keys are aligned with the Python ov.conf schema.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"
)

// Config is the top-level configuration object for ctxhub-server.
type Config struct {
	Server   ServerConfig   `mapstructure:"server"   validate:"required"`
	VLM      VLMConfig      `mapstructure:"vlm"`
	Embedder EmbedderConfig `mapstructure:"embedder"`
	Rerank   RerankConfig   `mapstructure:"rerank"`
	VectorDB VectorDBConfig `mapstructure:"vectordb" validate:"required"`
	RAGFS    RAGFSConfig    `mapstructure:"ragfs"    validate:"required"`
	Auth     AuthConfig     `mapstructure:"auth"`
	OAuth    OAuthConfig    `mapstructure:"oauth"`
	OTEL     OTELConfig     `mapstructure:"otel"`
	Bot      BotConfig      `mapstructure:"bot"`
	Queue    QueueConfig    `mapstructure:"queue"`
	Parse    ParseConfig    `mapstructure:"parse"`
	Prompts  PromptsConfig  `mapstructure:"prompts"`
}

// PromptsConfig configures the prompt template loader. Mirrors the
// Python openviking_cli.utils.config.PromptsConfig schema. When Dir
// is empty, the internal/prompts package loads its embedded archive;
// when set, templates are loaded from that on-disk directory and an
// fsnotify watcher can hot-reload changes.
type PromptsConfig struct {
	// Dir is the on-disk prompt templates directory. Empty means
	// use the embedded archive. When set, the directory must mirror
	// the embedded layout (compression/, vision/, etc.).
	Dir string `mapstructure:"dir"`
}

// ParseConfig configures the parse layer (accessors + parsers).
type ParseConfig struct {
	Feishu FeishuConfig `mapstructure:"feishu"`
}

// FeishuConfig configures the Feishu/Lark accessor (app credentials for
// the larksuite/oapi-sdk-go client). Empty AppID/AppSecret disables
// remote fetch; the accessor returns a clear configuration error.
type FeishuConfig struct {
	AppID     string `mapstructure:"app_id"`
	AppSecret string `mapstructure:"app_secret"`
	Domain    string `mapstructure:"domain"`
}

// ServerConfig configures the HTTP server.
type ServerConfig struct {
	Host           string     `mapstructure:"host"`
	Port           int        `mapstructure:"port"`
	Workers        int        `mapstructure:"workers"`
	UploadDir      string     `mapstructure:"upload_dir"`
	MaxUploadSize  int64      `mapstructure:"max_upload_size"`
	RequestTimeout int        `mapstructure:"request_timeout"`
	ReadTimeout    int        `mapstructure:"read_timeout"`
	WriteTimeout   int        `mapstructure:"write_timeout"`
	IdleTimeout    int        `mapstructure:"idle_timeout"`
	EnablePprof    bool       `mapstructure:"enable_pprof"`
	CORS           CORSConfig `mapstructure:"cors"`

	// StudioPath is the on-disk path to the web-studio static assets
	// directory. When non-empty, BuildApp wires an http.Dir at this
	// path as deps.StudioFS and serves it under /studio/*. When empty,
	// /studio/* returns 501 UNSUPPORTED. Embedding is intentionally
	// avoided so deployments can ship a single binary plus a shared
	// web-studio directory without rebuilding the server.
	StudioPath string `mapstructure:"studio_path"`

	// TempUpload configures the TempUploadStore used by the HTTP server
	// to buffer chunked uploads before ingestion. Mode "local" keeps
	// uploads in os.TempDir(); Mode "shared" places them in SharedDir
	// so multiple server replicas can see the same upload. Mirrors the
	// Python TempUploadConfig in openviking/server/config.py.
	TempUpload TempUploadConfig `mapstructure:"temp_upload"`

	// LocalInputGuard configures the LocalInputGuard that blocks HTTP
	// paths from directly accessing host filesystem (path traversal /
	// SSRF protection). Mirrors the Python local_input_guard module.
	LocalInputGuard LocalInputGuardConfig `mapstructure:"local_input_guard"`

	// APIKeysPath is the on-disk path to the JSON file storing hashed
	// API key records managed by /api/v1/admin/accounts/:id/api-keys.
	// When empty, BuildApp uses the default ./data/apikeys.json. The
	// file is created on first write (atomic tmpfile+rename).
	APIKeysPath string `mapstructure:"api_keys_path"`
}

// TempUploadConfig configures the TempUploadStore. Mode is "local" or
// "shared"; SharedDir is the shared directory used in shared mode
// (defaults to "./data/uploads/"); MaxBytes is the per-upload byte cap
// (defaults to 1 GiB).
type TempUploadConfig struct {
	Mode      string `mapstructure:"mode"`
	SharedDir string `mapstructure:"shared_dir"`
	MaxBytes  int64  `mapstructure:"max_bytes"`
}

// LocalInputGuardConfig configures the LocalInputGuard. AllowedRoots is
// the list of path prefixes that local sources are permitted to read
// from; defaults to ["./data/", "./tmp/"].
type LocalInputGuardConfig struct {
	AllowedRoots []string `mapstructure:"allowed_roots"`
}

// CORSConfig configures CORS.
type CORSConfig struct {
	Enabled          bool     `mapstructure:"enabled"`
	AllowOrigins     []string `mapstructure:"allow_origins"`
	AllowMethods     []string `mapstructure:"allow_methods"`
	AllowHeaders     []string `mapstructure:"allow_headers"`
	AllowCredentials bool     `mapstructure:"allow_credentials"`
	MaxAge           int      `mapstructure:"max_age"`
}

// Addr returns "host:port" for net/http.Server.
func (s ServerConfig) Addr() string { return fmt.Sprintf("%s:%d", s.Host, s.Port) }

// VLMConfig configures the vision-language model client.
type VLMConfig struct {
	Provider    string  `mapstructure:"provider"     validate:"omitempty,oneof=openai volcengine dashscope litellm codex glm kimi"`
	Model       string  `mapstructure:"model"`
	APIKey      string  `mapstructure:"api_key"`
	APIBase     string  `mapstructure:"api_base"`
	Temperature float64 `mapstructure:"temperature"`
	MaxRetries  int     `mapstructure:"max_retries"`
	Timeout     int     `mapstructure:"timeout"`
}

// EmbedderConfig configures the embedding provider.
type EmbedderConfig struct {
	Provider  string `mapstructure:"provider" validate:"omitempty,oneof=openai volcengine vikingdb dashscope cohere jina minimax voyage gemini litellm local"`
	Model     string `mapstructure:"model"`
	Dim       int    `mapstructure:"dim"        validate:"gte=0"`
	APIKey    string `mapstructure:"api_key"`
	APIBase   string `mapstructure:"api_base"`
	Timeout   int    `mapstructure:"timeout"`
	BatchSize int    `mapstructure:"batch_size"`
}

// RerankConfig configures the reranker.
type RerankConfig struct {
	Provider string `mapstructure:"provider" validate:"omitempty,oneof=cohere openai volcengine dashscope litellm"`
	Model    string `mapstructure:"model"`
	TopN     int    `mapstructure:"top_n"     validate:"gte=1"`
	APIKey   string `mapstructure:"api_key"`
	APIBase  string `mapstructure:"api_base"`
	Timeout  int    `mapstructure:"timeout"`
}

// VectorDBConfig configures the vector database backend.
//
// Backend selects one of: memory, local, qdrant, opengauss, volcengine,
// vikingdb, http. Each backend reads its own sub-struct; the shared
// CollectionPrefix is applied to every collection name via vectordb.CollectionName.
type VectorDBConfig struct {
	Backend          string            `mapstructure:"backend"           validate:"oneof=memory local qdrant opengauss volcengine vikingdb http"`
	CollectionPrefix string            `mapstructure:"collection_prefix"`
	Qdrant           QdrantConfig      `mapstructure:"qdrant"`
	OpenGauss        OpenGaussConfig   `mapstructure:"opengauss"`
	Local            LocalVectorConfig `mapstructure:"local"`
	HTTP             HTTPVectorConfig  `mapstructure:"http"`
	Volcengine       VolcengineConfig  `mapstructure:"volcengine"`
	VikingDB         VikingDBConfig    `mapstructure:"vikingdb"`
}

// QdrantConfig configures Qdrant client.
type QdrantConfig struct {
	URL    string `mapstructure:"url"`
	APIKey string `mapstructure:"api_key"`
}

// OpenGaussConfig configures pgx connection to OpenGauss (pgvector).
type OpenGaussConfig struct {
	DSN    string `mapstructure:"dsn"`
	Schema string `mapstructure:"schema"` // default "public"
}

// LocalVectorConfig configures on-disk hnswlib.
type LocalVectorConfig struct {
	Path string `mapstructure:"path"`
}

// HTTPVectorConfig configures a generic HTTP vector backend that proxies
// requests to any REST API exposing the seven CollectionAdapter operations.
// Each operation maps to a configurable URL; the per-op URLs override the
// generic URL when set. If a per-op URL is empty, the backend falls back to
// URL + "/" + op (e.g. URL + "/search"). APIKey is sent as
// "Authorization: Bearer <api_key>" when non-empty.
type HTTPVectorConfig struct {
	URL                 string `mapstructure:"url"`
	APIKey              string `mapstructure:"api_key"`
	EnsureCollectionURL string `mapstructure:"ensure_collection_url"`
	DropCollectionURL   string `mapstructure:"drop_collection_url"`
	ListCollectionsURL  string `mapstructure:"list_collections_url"`
	UpsertURL           string `mapstructure:"upsert_url"`
	SearchURL           string `mapstructure:"search_url"`
	DeleteURL           string `mapstructure:"delete_url"`
	GetURL              string `mapstructure:"get_url"`
	CountURL            string `mapstructure:"count_url"`
}

// VolcengineConfig configures the Volcengine Ark vector store client.
// Ark exposes an OpenAI-compatible /api/v3 endpoint; the vector store API
// follows the same auth (Bearer <api_key>) and base path.
type VolcengineConfig struct {
	BaseURL string `mapstructure:"base_url"` // default https://ark.cn-beijing.volces.com/api/v3
	APIKey  string `mapstructure:"api_key"`
	// StoreID is an optional pre-provisioned vector store ID. When empty,
	// EnsureCollection creates a new store via POST /vectorstores and caches
	// the returned ID per collection name.
	StoreID string `mapstructure:"store_id"`
}

// VikingDBConfig configures the Volcengine VikingDB client.
//
// VikingDB's Go SDK (github.com/volcengine/volcengine-go-sdk/service/vikingdb)
// at v1.2.x exposes only collection/index/task management — data operations
// (UpsertData/Search/DeleteData) are not in the public SDK surface, so the
// adapter issues signed HTTP requests to the VikingDB REST API directly.
//
// Auth uses Volcengine V4 HMAC-SHA256 signing with AccessKey/SecretKey.
// The V4 credential scope's "service" component is "air" (not "vikingdb"),
// matching the reference SDK at github.com/volcengine/volc-sdk-golang@v1.0.211
// service/vikingdb/vikingDBService.go:336. Region defaults to "cn-beijing"
// and Host to "api-vikingdb.volces.com" (the public endpoint used by the
// same reference SDK and coze-studio's integration tests).
type VikingDBConfig struct {
	Host      string `mapstructure:"host"`       // default api-vikingdb.volces.com
	Region    string `mapstructure:"region"`     // default cn-beijing
	AccessKey string `mapstructure:"access_key"` // Volcengine AK
	SecretKey string `mapstructure:"secret_key"` // Volcengine SK
}

// RAGFSConfig configures the resource filesystem.
type RAGFSConfig struct {
	Mounts     []MountConfig    `mapstructure:"mounts"`
	Cache      CacheConfig      `mapstructure:"cache"`
	MultiWrite MultiWriteConfig `mapstructure:"multi_write"`
	Redirect   RedirectConfig   `mapstructure:"redirect"`
}

// MountConfig describes one ragfs mount point.
type MountConfig struct {
	Name     string `mapstructure:"name"     validate:"required"`
	Backend  string `mapstructure:"backend"  validate:"oneof=local memory s3"`
	Path     string `mapstructure:"path"`
	Endpoint string `mapstructure:"endpoint"`
	Bucket   string `mapstructure:"bucket"`
	Region   string `mapstructure:"region"`
	ReadOnly bool   `mapstructure:"read_only"`
}

// CacheConfig configures the ragfs metadata cache.
type CacheConfig struct {
	Provider string      `mapstructure:"provider" validate:"oneof=memory redis"`
	Redis    RedisConfig `mapstructure:"redis"`
}

// RedisConfig configures Redis client.
type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

// MultiWriteConfig configures multi-write replication.
type MultiWriteConfig struct {
	Backups []string `mapstructure:"backups"`
	Sync    bool     `mapstructure:"sync"`
}

// RedirectConfig configures large-file redirect.
type RedirectConfig struct {
	FileOverSize int64 `mapstructure:"file_over_size"`
}

// AuthConfig configures authentication.
type AuthConfig struct {
	APIKey APIKeyAuthConfig `mapstructure:"api_key"`
	OAuth  bool             `mapstructure:"oauth"`
}

// APIKeyAuthConfig configures API key auth.
type APIKeyAuthConfig struct {
	Enabled  bool   `mapstructure:"enabled"`
	HashAlgo string `mapstructure:"hash_algorithm" validate:"oneof=argon2id bcrypt sha256"`
}

// OAuthConfig configures the OAuth 2.1 server (fosite).
type OAuthConfig struct {
	Issuer  string         `mapstructure:"issuer"`
	Clients []ClientConfig `mapstructure:"clients"`
	// Providers configures the external OAuth provider client flow
	// (feishu/google/slack/dingtalk). When non-empty, /api/v1/oauth/
	// authorize redirects to the named provider, /callback exchanges
	// the code, and /token?grant_type=refresh_token rotates the stored
	// token. Empty means only the fosite client_credentials grant is
	// available.
	Providers []OAuthProviderConfig `mapstructure:"providers"`
	// TokenStoreDSN is the SQLite DSN for the persistent provider-token
	// store. ":memory:" for in-memory (lost on restart), or a file
	// path. Empty defaults to ":memory:".
	TokenStoreDSN string `mapstructure:"token_store_dsn"`
	// Crypto configures the envelope-encryption provider used to
	// encrypt provider tokens at rest. When empty, tokens are stored
	// in plaintext (test-only; production must supply a real config).
	Crypto CryptoConfig `mapstructure:"crypto"`
}

// OAuthProviderConfig configures one external OAuth provider.
type OAuthProviderConfig struct {
	Name         string   `mapstructure:"name"          validate:"required,oneof=feishu google slack dingtalk"`
	ClientID     string   `mapstructure:"client_id"`
	ClientSecret string   `mapstructure:"client_secret"`
	RedirectURIs []string `mapstructure:"redirect_uris"`
	Scopes       []string `mapstructure:"scopes"`
	// AuthURL / TokenURL / UserInfoURL override the provider's default
	// endpoints. When empty, the provider adapter's production
	// endpoints are used. Tests override these to point at an
	// httptest server.
	AuthURL     string `mapstructure:"auth_url"`
	TokenURL    string `mapstructure:"token_url"`
	UserInfoURL string `mapstructure:"user_info_url"`
}

// CryptoConfig configures envelope encryption for secrets at rest.
// Mirrors internal/crypto.Config but kept in the config package so
// the OAuth config can be loaded from a single YAML file without a
// cross-package decode hook.
type CryptoConfig struct {
	Provider   string         `mapstructure:"provider"`
	Local      LocalCryptoConfig `mapstructure:"local"`
	Vault      VaultCryptoConfig `mapstructure:"vault"`
	Volcengine VolcengineCryptoConfig `mapstructure:"volcengine"`
}

// LocalCryptoConfig configures the local crypto provider.
type LocalCryptoConfig struct {
	MasterKeyPath string `mapstructure:"master_key_path"`
	Passphrase    string `mapstructure:"passphrase"`
}

// VaultCryptoConfig configures the Vault crypto provider.
type VaultCryptoConfig struct {
	Address   string `mapstructure:"address"`
	TokenPath string `mapstructure:"token_path"`
	KeyPath   string `mapstructure:"key_path"`
}

// VolcengineCryptoConfig configures the Volcengine KMS crypto provider.
type VolcengineCryptoConfig struct {
	Region    string `mapstructure:"region"`
	AccessKey string `mapstructure:"access_key"`
	SecretKey string `mapstructure:"secret_key"`
	KmsKeyID  string `mapstructure:"kms_key_id"`
}

// ClientConfig describes one OAuth client (DCR-registered).
type ClientConfig struct {
	ID         string   `mapstructure:"id"           validate:"required"`
	Secret     string   `mapstructure:"secret"`
	Name       string   `mapstructure:"name"`
	Scopes     []string `mapstructure:"scopes"`
	Redirects  []string `mapstructure:"redirects"`
	GrantTypes []string `mapstructure:"grant_types"`
	Public     bool     `mapstructure:"public"`
}

// OTELConfig configures OpenTelemetry.
type OTELConfig struct {
	Exporter    string  `mapstructure:"exporter"    validate:"oneof=memory otlp-grpc otlp-http"`
	Endpoint    string  `mapstructure:"endpoint"`
	ServiceName string  `mapstructure:"service_name"`
	SampleRate  float64 `mapstructure:"sample_rate" validate:"gte=0,lte=1"`
	LogLevel    string  `mapstructure:"log_level"   validate:"omitempty,oneof=debug info warn warning error"`
}

// BotConfig configures vikingbot.
type BotConfig struct {
	Enabled  bool                     `mapstructure:"enabled"`
	Channels map[string]ChannelConfig `mapstructure:"channels"`
}

// ChannelConfig describes one bot channel.
type ChannelConfig struct {
	Provider  string `mapstructure:"provider"  validate:"oneof=telegram feishu dingtalk slack qq"`
	Token     string `mapstructure:"token"`
	AppID     string `mapstructure:"app_id"`
	AppSecret string `mapstructure:"app_secret"`
	Endpoint  string `mapstructure:"endpoint"`
}

// QueueConfig configures queuefs (asynq).
type QueueConfig struct {
	Backend    string      `mapstructure:"backend"  validate:"oneof=memory redis"`
	Redis      RedisConfig `mapstructure:"redis"`
	Concurrent int         `mapstructure:"concurrent"`
}

// Load resolves configuration from defaults, files, env, and flags.
//
// path is an optional override pointing at a single ov.conf file. When
// non-empty it is consulted in addition to the standard search order
// (system, user, $OV_CONFIG_PATH); env vars (prefix OV_) still apply on top.
func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetEnvPrefix("OV")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	setDefaults(v)

	for _, p := range candidatePaths(path) {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err != nil {
			continue
		}
		v.SetConfigFile(p)
		// Force YAML; the canonical OpenViking config uses the .conf
		// extension which viper cannot infer a type from.
		v.SetConfigType("yaml")
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("config: read %s: %w", p, err)
		}
	}

	var cfg Config
	if err := v.Unmarshal(&cfg, viper.DecodeHook(decodeHooks())); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}
	applyDashscopeEnv(&cfg)
	if err := validator.New().Struct(&cfg); err != nil {
		return nil, fmt.Errorf("config: validate: %w", err)
	}
	return &cfg, nil
}

// applyDashscopeEnv fills in blank VLM/embedder/rerank/vectordb fields from
// DASHSCOPE_*, SAKER_MODEL_BASE_URL, ANTHROPIC_BASE_URL, and VOLCENGINE_*
// environment variables. Existing config-file values take precedence; only
// blank fields are populated so on-disk credentials win over shell env.
//
// Provider gating: env vars are only applied when the corresponding provider
// is a real remote backend (openai/dashscope/volcengine/etc.). The "local"
// embedder and empty/"" VLM provider are intentionally skipped — filling
// APIBase on local embedder triggers NewLocal's OpenAI sidecar path
// (local.go:54), which then fails with "model is required" because the
// local config has no model. Same trap for rerank's local fallback.
func applyDashscopeEnv(cfg *Config) {
	vlmProviders := map[string]bool{"openai": true, "dashscope": true, "volcengine": true, "codex": true, "glm": true, "kimi": true, "litellm": true, "anthropic": true}
	embedderProviders := map[string]bool{"openai": true, "dashscope": true, "volcengine": true, "cohere": true, "jina": true, "minimax": true, "voyage": true, "gemini": true, "litellm": true, "vikingdb": true}
	rerankProviders := map[string]bool{"cohere": true, "openai": true, "volcengine": true, "dashscope": true, "litellm": true}
	if vlmProviders[cfg.VLM.Provider] {
		if cfg.VLM.APIKey == "" {
			cfg.VLM.APIKey = os.Getenv("DASHSCOPE_API_KEY")
		}
		if cfg.VLM.APIBase == "" {
			if v := os.Getenv("SAKER_MODEL_BASE_URL"); v != "" {
				cfg.VLM.APIBase = v
			} else if v := os.Getenv("ANTHROPIC_BASE_URL"); v != "" {
				cfg.VLM.APIBase = v
			} else if v := os.Getenv("DASHSCOPE_BASE_URL"); v != "" {
				cfg.VLM.APIBase = v
			}
		}
	}
	if embedderProviders[cfg.Embedder.Provider] {
		if cfg.Embedder.APIKey == "" {
			cfg.Embedder.APIKey = os.Getenv("DASHSCOPE_API_KEY")
		}
		if cfg.Embedder.APIBase == "" {
			cfg.Embedder.APIBase = os.Getenv("DASHSCOPE_BASE_URL")
		}
	}
	if rerankProviders[cfg.Rerank.Provider] {
		if cfg.Rerank.APIKey == "" {
			cfg.Rerank.APIKey = os.Getenv("DASHSCOPE_API_KEY")
		}
		if cfg.Rerank.APIBase == "" {
			cfg.Rerank.APIBase = os.Getenv("DASHSCOPE_BASE_URL")
		}
	}
	if cfg.VectorDB.Backend == "vikingdb" {
		if cfg.VectorDB.VikingDB.AccessKey == "" {
			cfg.VectorDB.VikingDB.AccessKey = os.Getenv("VOLCENGINE_ACCESS_KEY")
		}
		if cfg.VectorDB.VikingDB.SecretKey == "" {
			cfg.VectorDB.VikingDB.SecretKey = os.Getenv("VOLCENGINE_SECRET_KEY")
		}
		if cfg.VectorDB.VikingDB.Region == "" {
			cfg.VectorDB.VikingDB.Region = os.Getenv("VOLCENGINE_REGION")
		}
	}
	if cfg.VectorDB.Backend == "volcengine" {
		if cfg.VectorDB.Volcengine.APIKey == "" {
			cfg.VectorDB.Volcengine.APIKey = os.Getenv("VOLCENGINE_ACCESS_KEY")
		}
	}
}

func candidatePaths(override string) []string {
	paths := []string{"/etc/openviking/ov.conf"}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".config", "openviking", "ov.conf"))
	}
	paths = append(paths, os.Getenv("OV_CONFIG_PATH"))
	if override != "" {
		paths = append(paths, override)
	}
	return paths
}
