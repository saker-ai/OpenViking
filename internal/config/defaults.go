package config

import (
	"reflect"
	"strconv"

	"github.com/mitchellh/mapstructure"
	"github.com/spf13/viper"
)

// setDefaults installs the built-in default values for every config key.
// Values mirror the Python ov.conf.example defaults.
func setDefaults(v *viper.Viper) {
	v.SetDefault("server.host", "0.0.0.0")
	v.SetDefault("server.port", 8080)
	v.SetDefault("server.workers", runtimeNumCPU())
	v.SetDefault("server.upload_dir", "/tmp/ov-uploads")
	v.SetDefault("server.max_upload_size", int64(1<<30))
	v.SetDefault("server.request_timeout", 300)
	v.SetDefault("server.read_timeout", 60)
	v.SetDefault("server.write_timeout", 600)
	v.SetDefault("server.idle_timeout", 120)
	v.SetDefault("server.enable_pprof", false)
	v.SetDefault("server.cors.enabled", true)
	v.SetDefault("server.cors.allow_origins", []string{"*"})
	v.SetDefault("server.cors.allow_methods", []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"})
	v.SetDefault("server.cors.allow_headers", []string{"*"})
	v.SetDefault("server.cors.allow_credentials", false)
	v.SetDefault("server.cors.max_age", 600)
	v.SetDefault("server.api_keys_path", "./data/apikeys.json")

	v.SetDefault("server.temp_upload.mode", "local")
	v.SetDefault("server.temp_upload.shared_dir", "./data/uploads/")
	v.SetDefault("server.temp_upload.max_bytes", int64(1<<30))
	v.SetDefault("server.local_input_guard.allowed_roots", []string{"./data/", "./tmp/"})

	v.SetDefault("vlm.temperature", 0.0)
	v.SetDefault("vlm.max_retries", 2)
	v.SetDefault("vlm.timeout", 300)

	v.SetDefault("embedder.batch_size", 64)
	v.SetDefault("embedder.timeout", 120)

	v.SetDefault("rerank.top_n", 5)
	v.SetDefault("rerank.timeout", 60)

	v.SetDefault("vectordb.backend", "qdrant")
	v.SetDefault("vectordb.collection_prefix", "ov_")
	v.SetDefault("vectordb.local.path", "./data/vectordb")
	v.SetDefault("vectordb.opengauss.schema", "public")
	v.SetDefault("vectordb.volcengine.base_url", "https://ark.cn-beijing.volces.com/api/v3")
	v.SetDefault("vectordb.vikingdb.region", "cn-north-1")

	v.SetDefault("ragfs.cache.provider", "memory")
	v.SetDefault("ragfs.multi_write.sync", true)
	v.SetDefault("ragfs.redirect.file_over_size", int64(100<<20))

	v.SetDefault("auth.api_key.enabled", true)
	v.SetDefault("auth.api_key.hash_algorithm", "argon2id")
	v.SetDefault("auth.oauth", false)

	v.SetDefault("oauth.issuer", "")

	v.SetDefault("otel.exporter", "memory")
	v.SetDefault("otel.service_name", "openviking-server")
	v.SetDefault("otel.sample_rate", 1.0)
	v.SetDefault("otel.log_level", "info")

	v.SetDefault("bot.enabled", false)

	v.SetDefault("queue.backend", "memory")
	v.SetDefault("queue.concurrent", 8)

	v.SetDefault("parse.feishu.domain", "https://open.feishu.cn")

	// Empty prompts.dir means use the embedded template archive; the
	// on-disk override is opt-in for hot-reload development.
	v.SetDefault("prompts.dir", "")
}

// decodeHooks composes the standard mapstructure decode hooks with extra
// conversions for string-to-int / int64 (viper's default only handles
// time.Duration and slice).
func decodeHooks() mapstructure.DecodeHookFunc {
	return mapstructure.ComposeDecodeHookFunc(
		mapstructure.StringToTimeDurationHookFunc(),
		mapstructure.StringToSliceHookFunc(","),
		stringToInt,
		stringToInt64,
	)
}

// stringToInt converts "123" -> 123 when the target field is int.
func stringToInt(f reflect.Type, t reflect.Type, data interface{}) (interface{}, error) {
	if t.Kind() != reflect.Int {
		return data, nil
	}
	switch s := data.(type) {
	case string:
		if s == "" {
			return 0, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return nil, err
		}
		return n, nil
	}
	return data, nil
}

// stringToInt64 converts "123" -> int64(123) when the target field is int64.
func stringToInt64(f reflect.Type, t reflect.Type, data interface{}) (interface{}, error) {
	if t.Kind() != reflect.Int64 {
		return data, nil
	}
	switch s := data.(type) {
	case string:
		if s == "" {
			return int64(0), nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, err
		}
		return n, nil
	}
	return data, nil
}

// runtimeNumCPU returns the available parallelism, capped at 16 for sane
// defaults on huge boxes; users can override via config.
func runtimeNumCPU() int {
	n := runtimeGOMAXPROCS()
	if n > 16 {
		n = 16
	}
	if n < 1 {
		n = 1
	}
	return n
}
