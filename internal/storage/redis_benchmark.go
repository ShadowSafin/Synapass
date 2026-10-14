package storage

import (
	"context"
	"time"
)

// Benchmark queue primitives over Redis lists.
//
// Keys are namespaced with the deployment prefix so one Redis can serve
// several deployments without cross-talk. The benchmark engine uses these
// through its minimal RedisList interface; workers on every replica drain
// the same list, which is what makes the pool horizontally scalable.

// BenchmarkLPush prepends run IDs to the benchmark queue.
func (r *Redis) BenchmarkLPush(ctx context.Context, key string, values ...string) (int64, error) {
	if r == nil || r.client == nil {
		return 0, errRedisUnavailable
	}
	full := r.Prefix() + ":" + key
	args := make([]any, 0, len(values))
	for _, v := range values {
		args = append(args, v)
	}
	return r.client.LPush(ctx, full, args...).Result()
}

// BenchmarkBRPop removes and returns the next run ID, waiting up to timeout.
func (r *Redis) BenchmarkBRPop(ctx context.Context, timeout time.Duration, key string) (string, error) {
	if r == nil || r.client == nil {
		return "", errRedisUnavailable
	}
	full := r.Prefix() + ":" + key
	secs := timeout.Seconds()
	res, err := r.client.BRPop(ctx, time.Duration(secs*float64(time.Second)), full).Result()
	if err != nil {
		return "", err
	}
	if len(res) == 2 {
		return res[1], nil
	}
	return "", errRedisUnavailable
}

// BenchmarkLLen reports the queue depth.
func (r *Redis) BenchmarkLLen(ctx context.Context, key string) (int64, error) {
	if r == nil || r.client == nil {
		return 0, errRedisUnavailable
	}
	return r.client.LLen(ctx, r.Prefix()+":"+key).Result()
}

var errRedisUnavailable = redisUnavailableError{}

type redisUnavailableError struct{}

func (redisUnavailableError) Error() string { return "redis is unavailable" }
