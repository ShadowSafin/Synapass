package platcache

import (
	"context"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisAdapter adapts a *redis.Client to the Store interface with the
// cache's key prefix. All errors propagate so the Cache can fall back to
// DB gracefully; nothing here ever panics on a Redis outage.
type RedisAdapter struct {
	client *redis.Client
	prefix string
}

// NewRedisAdapter builds a Store over client. A nil client yields a nil
// Store (pass nil to New instead).
func NewRedisAdapter(client *redis.Client, prefix string) *RedisAdapter {
	if client == nil {
		return nil
	}
	if prefix == "" {
		prefix = "synapass"
	}
	return &RedisAdapter{client: client, prefix: prefix}
}

func (a *RedisAdapter) key(k string) string { return a.prefix + ":" + k }

func (a *RedisAdapter) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if a == nil || a.client == nil {
		return nil, false, nil
	}
	b, err := a.client.Get(ctx, a.key(key)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func (a *RedisAdapter) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if a == nil || a.client == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = 60 * time.Second
	}
	return a.client.Set(ctx, a.key(key), value, ttl).Err()
}

func (a *RedisAdapter) Delete(ctx context.Context, key string) error {
	if a == nil || a.client == nil {
		return nil
	}
	return a.client.Del(ctx, a.key(key)).Err()
}

func (a *RedisAdapter) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if a == nil || a.client == nil {
		return 0, nil
	}
	pattern := a.key(prefix) + "*"
	var (
		cursor  uint64
		removed int
	)
	for {
		keys, next, err := a.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return removed, err
		}
		if len(keys) > 0 {
			if err := a.client.Del(ctx, keys...).Err(); err != nil {
				return removed, err
			}
			removed += len(keys)
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return removed, nil
}

func (a *RedisAdapter) SetNX(ctx context.Context, key string, value []byte, ttl time.Duration) (bool, error) {
	if a == nil || a.client == nil {
		return true, nil
	}
	if ttl <= 0 {
		ttl = 10 * time.Second
	}
	return a.client.SetNX(ctx, a.key(key), value, ttl).Result()
}

// Health reports Redis reachability for dashboards.
func (a *RedisAdapter) Health(ctx context.Context) (bool, time.Duration, error) {
	if a == nil || a.client == nil {
		return false, 0, nil
	}
	start := time.Now()
	if err := a.client.Ping(ctx).Err(); err != nil {
		return false, time.Since(start), err
	}
	return true, time.Since(start), nil
}
