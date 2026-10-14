package bootstrap

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/shadowsafin/synapass/internal/benchmark"
	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/storage"
)

// benchmarkRedis adapts storage.Redis to the engine's minimal queue interface.
type benchmarkRedis struct {
	redis *storage.Redis
}

// LPush implements benchmark.RedisList.
func (b benchmarkRedis) LPush(ctx context.Context, key string, values ...string) (int64, error) {
	return b.redis.BenchmarkLPush(ctx, key, values...)
}

// BRPop implements benchmark.RedisList.
func (b benchmarkRedis) BRPop(ctx context.Context, timeout time.Duration, keys ...string) (string, error) {
	if len(keys) == 0 {
		return "", nil
	}
	return b.redis.BenchmarkBRPop(ctx, timeout, keys[0])
}

// LLen implements benchmark.RedisList.
func (b benchmarkRedis) LLen(ctx context.Context, key string) (int64, error) {
	return b.redis.BenchmarkLLen(ctx, key)
}

// BuildBenchmark constructs the Autonomous Benchmark Engine.
//
// It always returns an engine handle (so the API can report enabled=false),
// but the engine only runs workers and the scheduler when the master switch
// is on. When disabled the handle is inert: no goroutines, no Redis traffic,
// zero impact on the inference gateway.
//
// The effective switch is the configuration flag OR the persisted dashboard
// toggle (`benchmark_enabled` in the settings table), so enabling the engine
// from the Benchmark page survives restarts without a config change. Both
// default to off.
func BuildBenchmark(cfg *config.Config, repos *storage.Repositories, redis *storage.Redis, logger *slog.Logger) *benchmark.Engine {
	if logger == nil {
		logger = slog.Default()
	}
	var queueBackend benchmark.RedisList
	if redis != nil {
		queueBackend = benchmarkRedis{redis: redis}
	}
	eng := benchmark.New(benchmark.Options{
		Config: cfg.Benchmark,
		Logger: logger,
		Repos:  repos,
		Redis:  queueBackend,
	})
	// Restore the persisted dashboard toggle. Best-effort and bounded: a
	// settings read failure keeps the safe default (disabled) rather than
	// blocking gateway startup on an enterprise feature.
	if repos != nil && repos.Settings != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if raw, ok, err := repos.Settings.Get(ctx, benchmarkEnabledSetting); err != nil {
			logger.Debug("benchmark toggle restore failed; staying disabled", "error", err)
		} else if ok {
			var enabled bool
			if jerr := json.Unmarshal([]byte(raw), &enabled); jerr == nil && enabled {
				eng.SetRuntimeEnabled(true)
				logger.Info("benchmark engine enabled by persisted dashboard toggle")
			}
		}
		cancel()
	}
	return eng
}

// benchmarkEnabledSetting is the settings-table key for the dashboard toggle.
const benchmarkEnabledSetting = "benchmark_enabled"
