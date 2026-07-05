package cache

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/saker-ai/ctxhub/internal/config"
)

// Redis is a Redis-backed metadata cache. It uses go-redis v9 and the
// standard SET/GET/DEL/EXPIRE commands. Corresponds to the Rust
// ragfs-cache-redis backend.
type Redis struct {
	client redis.UniversalClient
	prefix string
}

// NewRedis constructs a Redis cache from a config.RedisConfig. The prefix
// is applied to every key so multiple OpenViking deployments can share a
// Redis instance without colliding.
func NewRedis(cfg config.RedisConfig, prefix string) *Redis {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
	})
	return &Redis{client: client, prefix: prefix}
}

// NewRedisWithClient is provided for testing and injection. The caller
// retains ownership of cleanup (e.g. Close).
func NewRedisWithClient(client redis.UniversalClient, prefix string) *Redis {
	return &Redis{client: client, prefix: prefix}
}

// Close releases the Redis connection.
func (r *Redis) Close() error {
	if r.client == nil {
		return nil
	}
	return r.client.Close()
}

// Get returns the cached bytes for key. Returns ErrCacheMiss on miss.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, error) {
	if r.client == nil {
		return nil, ErrCacheMiss
	}
	val, err := r.client.Get(ctx, r.k(key)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return nil, ErrCacheMiss
		}
		return nil, err
	}
	return val, nil
}

// Put stores val under key with the given ttl. A ttl of 0 means "no
// expiry"; Redis interprets a ttl of 0 as "keep forever" via SET K V 0.
func (r *Redis) Put(ctx context.Context, key string, val []byte, ttl time.Duration) error {
	if r.client == nil {
		return nil
	}
	if ttl <= 0 {
		return r.client.Set(ctx, r.k(key), val, 0).Err()
	}
	return r.client.Set(ctx, r.k(key), val, ttl).Err()
}

// Delete removes key. Missing keys are not an error.
func (r *Redis) Delete(ctx context.Context, key string) error {
	if r.client == nil {
		return nil
	}
	return r.client.Del(ctx, r.k(key)).Err()
}

// BatchGet returns the available keys in one MGET pass.
func (r *Redis) BatchGet(ctx context.Context, keys []string) (map[string][]byte, error) {
	if r.client == nil {
		return map[string][]byte{}, nil
	}
	if len(keys) == 0 {
		return map[string][]byte{}, nil
	}
	args := make([]string, len(keys))
	for i, k := range keys {
		args[i] = r.k(k)
	}
	vals, err := r.client.MGet(ctx, args...).Result()
	if err != nil {
		return nil, err
	}
	out := make(map[string][]byte, len(keys))
	for i, v := range vals {
		if v == nil {
			continue
		}
		if b, ok := v.(string); ok {
			out[keys[i]] = []byte(b)
		} else if b, ok := v.([]byte); ok {
			out[keys[i]] = b
		}
	}
	return out, nil
}

// BatchPut stores all items with the same ttl via MSET + pipeline EXPIRE.
func (r *Redis) BatchPut(ctx context.Context, items map[string][]byte, ttl time.Duration) error {
	if r.client == nil {
		return nil
	}
	if len(items) == 0 {
		return nil
	}
	pipe := r.client.Pipeline()
	for k, v := range items {
		pipe.Set(ctx, r.k(k), v, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

// k prefixes the key with the deployment namespace.
func (r *Redis) k(key string) string {
	if r.prefix == "" {
		return key
	}
	return r.prefix + ":" + key
}
