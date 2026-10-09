package storage

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/domain"
)

// Redis provides the operational data layer: credential caching, rate limiting,
// budget counters, distributed locks and in-flight request coordination.
//
// Everything stored here is reconstructible. Redis is never the system of record;
// it holds data whose loss is a performance or convenience problem rather than a
// correctness problem. That constraint is what allows Redis to be optional at
// startup: a deployment without it degrades to in-process limiting and no
// credential cache.
type Redis struct {
	client *redis.Client
	prefix string
	logger *slog.Logger
}

// NewRedis connects to Redis.
//
// When cfg.Required is false and the connection fails, an error is returned but
// the caller is expected to continue with a nil client and a degraded-mode
// limiter. Returning the error rather than swallowing it keeps the decision at the
// wiring layer where the fallback behaviour actually lives.
func NewRedis(ctx context.Context, cfg config.RedisConfig, logger *slog.Logger) (*Redis, error) {
	if cfg.Addr == "" && len(cfg.SentinelAddrs) == 0 {
		return nil, fmt.Errorf("redis address is empty")
	}
	if logger == nil {
		logger = slog.Default()
	}

	var client *redis.Client
	if len(cfg.SentinelAddrs) > 0 {
		client = redis.NewFailoverClient(&redis.FailoverOptions{
			MasterName:    cfg.MasterName,
			SentinelAddrs: cfg.SentinelAddrs,
			Username:      cfg.Username,
			Password:      cfg.Password,
			DB:            cfg.DB,
			PoolSize:      cfg.PoolSize,
			DialTimeout:   cfg.DialTimeout.Std(),
			ReadTimeout:   cfg.ReadTimeout.Std(),
			WriteTimeout:  cfg.WriteTimeout.Std(),
			TLSConfig:     tlsConfig(cfg.TLS),
		})
	} else {
		client = redis.NewClient(&redis.Options{
			Addr:         cfg.Addr,
			Username:     cfg.Username,
			Password:     cfg.Password,
			DB:           cfg.DB,
			PoolSize:     cfg.PoolSize,
			DialTimeout:  cfg.DialTimeout.Std(),
			ReadTimeout:  cfg.ReadTimeout.Std(),
			WriteTimeout: cfg.WriteTimeout.Std(),
			TLSConfig:    tlsConfig(cfg.TLS),
		})
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("connect to redis at %s: %w", cfg.Addr, err)
	}

	logger.Info("connected to redis", "addr", cfg.Addr, "pool_size", cfg.PoolSize)

	prefix := cfg.KeyPrefix
	if prefix == "" {
		prefix = "synapass"
	}
	return &Redis{client: client, prefix: prefix, logger: logger}, nil
}

// tlsConfig returns a TLS configuration when enabled.
func tlsConfig(enabled bool) *tls.Config {
	if !enabled {
		return nil
	}
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

// Client exposes the underlying client for advanced use.
func (r *Redis) Client() *redis.Client { return r.client }

// Prefix reports the configured key prefix (default "synapass").
func (r *Redis) Prefix() string {
	if r == nil || r.prefix == "" {
		return "synapass"
	}
	return r.prefix
}

// Close releases the connection.
func (r *Redis) Close() error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Close()
}

// Ping verifies connectivity.
func (r *Redis) Ping(ctx context.Context) error {
	if r == nil || r.client == nil {
		return fmt.Errorf("redis is not initialized")
	}
	return r.client.Ping(ctx).Err()
}

// Stat renders pool statistics for the metrics endpoint.
func (r *Redis) Stat() map[string]any {
	if r == nil || r.client == nil {
		return nil
	}
	s := r.client.PoolStats()
	return map[string]any{
		"hits":        s.Hits,
		"misses":      s.Misses,
		"timeouts":    s.Timeouts,
		"total_conns": s.TotalConns,
		"idle_conns":  s.IdleConns,
		"stale_conns": s.StaleConns,
	}
}

// key namespaces a key.
func (r *Redis) key(parts ...string) string {
	out := r.prefix
	for _, p := range parts {
		out += ":" + p
	}
	return out
}

// ---------------------------------------------------------------------------
// auth.Cache
// ---------------------------------------------------------------------------

// Get implements auth.Cache.
func (r *Redis) Get(ctx context.Context, key string) ([]byte, bool, error) {
	if r == nil || r.client == nil {
		return nil, false, nil
	}
	value, err := r.client.Get(ctx, r.key(key)).Bytes()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return value, true, nil
}

// Set implements auth.Cache.
func (r *Redis) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if r == nil || r.client == nil {
		return nil
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return r.client.Set(ctx, r.key(key), value, ttl).Err()
}

// Delete implements auth.Cache.
func (r *Redis) Delete(ctx context.Context, key string) error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Del(ctx, r.key(key)).Err()
}

// DeletePrefix removes every key under a prefix.
//
// SCAN is used rather than KEYS because KEYS blocks the server for the duration of
// the scan, which on a production instance with millions of keys is an outage.
func (r *Redis) DeletePrefix(ctx context.Context, prefix string) (int, error) {
	if r == nil || r.client == nil {
		return 0, nil
	}
	pattern := r.key(prefix) + "*"
	var (
		cursor  uint64
		removed int
	)
	for {
		keys, next, err := r.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return removed, err
		}
		if len(keys) > 0 {
			if err := r.client.Del(ctx, keys...).Err(); err != nil {
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

// ---------------------------------------------------------------------------
// policy.CounterStore
// ---------------------------------------------------------------------------

// incrementSpendScript atomically adds an amount and sets the period expiry.
//
// A Lua script is required rather than INCRBYFLOAT followed by EXPIRE: between the
// two commands a crash would leave a counter with no expiry, which would silently
// make the budget permanent and block the tenant forever.
var incrementSpendScript = redis.NewScript(`
local key = KEYS[1]
local amount = ARGV[1]
local ttl = tonumber(ARGV[2])
local total = redis.call('INCRBYFLOAT', key, amount)
if redis.call('TTL', key) < 0 then
  redis.call('PEXPIRE', key, ttl)
end
return total
`)

// IncrementSpend implements policy.CounterStore.
//
// periodKey is the already-rendered period label, so the counter key is a pure
// function of the scope and the label. Rendering the label in the caller is what
// guarantees a charge and a later budget check address the same key.
func (r *Redis) IncrementSpend(ctx context.Context, scopeKey string, amount float64, periodKey string) (float64, error) {
	if r == nil || r.client == nil {
		return 0, nil
	}
	// The TTL is generous relative to the period so reconciliation has time to run
	// before the counter expires.
	ttl := periodTTL(periodKey)
	result, err := incrementSpendScript.Run(ctx, r.client,
		[]string{r.key("budget", scopeKey, periodKey)},
		amount, ttl.Milliseconds()).Float64()
	if err != nil {
		return 0, err
	}
	return result, nil
}

// GetSpend implements policy.CounterStore.
func (r *Redis) GetSpend(ctx context.Context, scopeKey, periodKey string) (float64, error) {
	if r == nil || r.client == nil {
		return 0, nil
	}
	value, err := r.client.Get(ctx, r.key("budget", scopeKey, periodKey)).Result()
	if err == redis.Nil {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseFloat(value, 64)
}

// periodTTL returns how long a spend counter should live, derived from the period
// label's prefix rather than from a constant. That keeps the storage layer
// independent of the policy package's period vocabulary while still expiring a
// counter well after its period has ended.
func periodTTL(periodKey string) time.Duration {
	switch {
	case len(periodKey) > 0 && periodKey[0] == 'd':
		return 48 * time.Hour
	case len(periodKey) > 0 && periodKey[0] == 'w':
		return 9 * 24 * time.Hour
	case len(periodKey) > 0 && periodKey[0] == 'm':
		return 35 * 24 * time.Hour
	default:
		return 400 * 24 * time.Hour
	}
}

// allowScript implements a fixed-window counter atomically.
//
// A fixed window is chosen over a sliding window or a token bucket because it is
// the only scheme whose behaviour an operator can predict exactly: "600 per
// minute" means at most 600 increments per clock minute, with no burst allowance
// that would let 1200 through across a boundary. Predictability wins for a limit
// that appears on a customer's contract.
var allowScript = redis.NewScript(`
local key = KEYS[1]
local limit = tonumber(ARGV[1])
local ttl = tonumber(ARGV[2])
local current = redis.call('INCR', key)
if current == 1 then
  redis.call('PEXPIRE', key, ttl)
end
local remaining_ttl = redis.call('PTTL', key)
if current > limit then
  return {0, 0, remaining_ttl}
end
return {1, limit - current, remaining_ttl}
`)

// AllowRequest implements policy.CounterStore.
func (r *Redis) AllowRequest(ctx context.Context, bucketKey string, limit int, window time.Duration) (bool, int, time.Time, error) {
	if r == nil || r.client == nil {
		return true, limit, time.Now().Add(window), nil
	}
	if limit <= 0 {
		return true, 0, time.Now().Add(window), nil
	}

	// The window is aligned to the wall clock so every replica shares the same
	// boundaries without coordination.
	now := time.Now().UTC()
	windowStart := now.Truncate(window)
	key := r.key("ratelimit", bucketKey, strconv.FormatInt(windowStart.Unix(), 10))

	result, err := allowScript.Run(ctx, r.client, []string{key}, limit, window.Milliseconds()).Slice()
	if err != nil {
		return false, 0, time.Time{}, err
	}
	if len(result) < 3 {
		return false, 0, time.Time{}, fmt.Errorf("unexpected rate limit script result")
	}

	allowed := toInt(result[0]) == 1
	remaining := toInt(result[1])
	ttlMS := toInt(result[2])

	resetAt := windowStart.Add(window)
	if ttlMS > 0 {
		resetAt = now.Add(time.Duration(ttlMS) * time.Millisecond)
	}
	return allowed, remaining, resetAt, nil
}

// toInt converts a Redis reply value to an integer.
func toInt(value any) int {
	switch v := value.(type) {
	case int64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(v)
		return n
	case float64:
		return int(v)
	default:
		return 0
	}
}

// ---------------------------------------------------------------------------
// Distributed coordination
// ---------------------------------------------------------------------------

// releaseLockScript deletes a lock only when the caller still owns it.
//
// Comparing the token before deleting prevents the classic bug where a caller
// whose lock already expired deletes the lock a different caller has since taken.
var releaseLockScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

// Lock is a held distributed lock.
type Lock struct {
	redis *Redis
	key   string
	token string
	ttl   time.Duration
}

// Acquire takes a distributed lock.
//
// The lock is used for leader election among replicas: the health probe loop and
// the retention pruner must run once cluster-wide, not once per replica. A TTL is
// mandatory so a crashed holder cannot deadlock the others.
func (r *Redis) Acquire(ctx context.Context, name string, ttl time.Duration) (*Lock, bool, error) {
	if r == nil || r.client == nil {
		// Without Redis, every replica behaves as the leader. That is correct for
		// a single-replica deployment, which is the only case where Redis is
		// absent, and it is better than silently disabling background work.
		return &Lock{redis: r, key: name, token: "no-redis", ttl: ttl}, true, nil
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}

	token := domain.NewID()
	ok, err := r.client.SetNX(ctx, r.key("lock", name), token, ttl).Result()
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	return &Lock{redis: r, key: r.key("lock", name), token: token, ttl: ttl}, true, nil
}

// Release frees the lock.
func (l *Lock) Release(ctx context.Context) error {
	if l == nil || l.redis == nil || l.redis.client == nil {
		return nil
	}
	return releaseLockScript.Run(ctx, l.redis.client, []string{l.key}, l.token).Err()
}

// Refresh extends the lock, so a long-running leader can keep its lease.
func (l *Lock) Refresh(ctx context.Context) error {
	if l == nil || l.redis == nil || l.redis.client == nil {
		return nil
	}
	return l.redis.client.Expire(ctx, l.key, l.ttl).Err()
}

// ---------------------------------------------------------------------------
// In-flight request coordination and response caching
// ---------------------------------------------------------------------------

// MarkInFlight records a request as being processed and reports whether this call
// created the marker.
//
// Used to suppress duplicate work: when a client retries a request that is still
// running, the gateway can either wait for the original or serve it separately.
// Phase 1 records the marker and exposes it to the admin API, which is enough to
// diagnose duplicate traffic; the cache and dedup behaviour that use it are
// deliberately deferred.
func (r *Redis) MarkInFlight(ctx context.Context, requestID string, ttl time.Duration) (bool, error) {
	if r == nil || r.client == nil {
		return true, nil
	}
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return r.client.SetNX(ctx, r.key("inflight", requestID), time.Now().Unix(), ttl).Result()
}

// ClearInFlight removes a request marker.
func (r *Redis) ClearInFlight(ctx context.Context, requestID string) error {
	if r == nil || r.client == nil {
		return nil
	}
	return r.client.Del(ctx, r.key("inflight", requestID)).Err()
}

// InFlightCount returns the number of marked in-flight requests.
func (r *Redis) InFlightCount(ctx context.Context) (int64, error) {
	if r == nil || r.client == nil {
		return 0, nil
	}
	var (
		cursor uint64
		count  int64
	)
	pattern := r.key("inflight") + "*"
	for {
		keys, next, err := r.client.Scan(ctx, cursor, pattern, 500).Result()
		if err != nil {
			return count, err
		}
		count += int64(len(keys))
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return count, nil
}

// GetResponseCache reads a cached response body.
func (r *Redis) GetResponseCache(ctx context.Context, cacheKey string) ([]byte, bool, error) {
	return r.Get(ctx, "response:"+cacheKey)
}

// SetResponseCache stores a response body.
func (r *Redis) SetResponseCache(ctx context.Context, cacheKey string, body []byte, ttl time.Duration) error {
	return r.Set(ctx, "response:"+cacheKey, body, ttl)
}

// SetProviderHealth caches a provider health assessment so replicas share one view
// without each probing independently.
func (r *Redis) SetProviderHealth(ctx context.Context, providerID string, payload []byte, ttl time.Duration) error {
	return r.Set(ctx, "health:"+providerID, payload, ttl)
}

// GetProviderHealth reads a cached health assessment.
func (r *Redis) GetProviderHealth(ctx context.Context, providerID string) ([]byte, bool, error) {
	return r.Get(ctx, "health:"+providerID)
}

// Info returns a small subset of server information for the admin system page.
func (r *Redis) Info(ctx context.Context) (map[string]string, error) {
	if r == nil || r.client == nil {
		return nil, nil
	}
	raw, err := r.client.Info(ctx, "server", "clients", "memory").Result()
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	// The INFO payload is a set of "key:value" lines grouped by section headers.
	for _, line := range splitLines(raw) {
		if line == "" || line[0] == '#' {
			continue
		}
		k, v, ok := cutColon(line)
		if !ok {
			continue
		}
		switch k {
		case "redis_version", "uptime_in_seconds", "connected_clients",
			"used_memory_human", "maxmemory_human", "redis_mode":
			out[k] = v
		}
	}
	return out, nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			line := s[start:i]
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			out = append(out, line)
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func cutColon(s string) (string, string, bool) {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i], s[i+1:], true
		}
	}
	return "", "", false
}
