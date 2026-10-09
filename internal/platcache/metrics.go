package platcache

import (
	"log/slog"
	"sync/atomic"
	"time"
)

// Stats is the observable cache state: hits, misses, stale hits,
// refreshes, invalidations, lock waits, DB fallbacks, and latency.
type Stats struct {
	Hits             int64
	Misses           int64
	StaleHits        int64
	Refreshes        int64
	RefreshFailures  int64
	Invalidations    int64
	InvalidatedKeys  int64
	LockWaits        int64
	LockTimeouts     int64
	DBFallbacks      int64
	Coalesced        int64
	StoreErrors      int64
	Prewarms         int64
	RouteResolutions int64
	// ResolutionLatencyMS is the rolling mean of resolve latency.
	ResolutionLatencyMS float64
	resolutionSamples   int64
}

// Snapshot returns a copy safe to export to dashboards.
func (s *Stats) Snapshot() Stats {
	if s == nil {
		return Stats{}
	}
	return Stats{
		Hits: s.Hits, Misses: s.Misses, StaleHits: s.StaleHits,
		Refreshes: s.Refreshes, RefreshFailures: s.RefreshFailures,
		Invalidations: s.Invalidations, InvalidatedKeys: s.InvalidatedKeys,
		LockWaits: s.LockWaits, LockTimeouts: s.LockTimeouts,
		DBFallbacks: s.DBFallbacks, Coalesced: s.Coalesced,
		StoreErrors: s.StoreErrors, Prewarms: s.Prewarms,
		RouteResolutions:    s.RouteResolutions,
		ResolutionLatencyMS: s.ResolutionLatencyMS,
	}
}

// HitRate is fresh+stale hits over all lookups.
func (s Stats) HitRate() float64 {
	total := s.Hits + s.StaleHits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits+s.StaleHits) / float64(total)
}

// Observer receives structured cache events for logs/metrics.
// The default is a slog-backed observer; wire a Prometheus observer in
// production to feed dashboards.
type Observer interface {
	OnHit(key string, kind Kind, tenant, layer string)
	OnMiss(key string, kind Kind, tenant, reason string)
	OnStaleHit(key string, kind Kind, tenant string)
	OnRefresh(key string, kind Kind, tenant string, err error)
	OnInvalidate(keys []string, tags []string, reason string, removed int)
	OnStampedeCoalesced(key string)
	OnFallback(key string, err error)
	OnResolve(latency time.Duration, cached bool)
}

// SlogObserver logs cache events at debug/info level.
type SlogObserver struct{ Logger *slog.Logger }

func (o *SlogObserver) log() *slog.Logger {
	if o == nil || o.Logger == nil {
		return slog.Default()
	}
	return o.Logger
}

func (o *SlogObserver) OnHit(key string, kind Kind, tenant, layer string) {
	o.log().Debug("platcache hit", "key", key, "kind", string(kind), "tenant", tenant, "layer", layer)
}
func (o *SlogObserver) OnMiss(key string, kind Kind, tenant, reason string) {
	o.log().Debug("platcache miss", "key", key, "kind", string(kind), "tenant", tenant, "reason", reason)
}
func (o *SlogObserver) OnStaleHit(key string, kind Kind, tenant string) {
	o.log().Info("platcache stale hit", "key", key, "kind", string(kind), "tenant", tenant)
}
func (o *SlogObserver) OnRefresh(key string, kind Kind, tenant string, err error) {
	if err != nil {
		o.log().Warn("platcache refresh failed", "key", key, "kind", string(kind), "tenant", tenant, "error", err)
		return
	}
	o.log().Debug("platcache refreshed", "key", key, "kind", string(kind), "tenant", tenant)
}
func (o *SlogObserver) OnInvalidate(keys []string, tags []string, reason string, removed int) {
	o.log().Info("platcache invalidated", "keys", keys, "tags", tags, "reason", reason, "removed", removed)
}
func (o *SlogObserver) OnStampedeCoalesced(key string) {
	o.log().Debug("platcache coalesced rebuild", "key", key)
}
func (o *SlogObserver) OnFallback(key string, err error) {
	o.log().Warn("platcache store fallback to DB", "key", key, "error", err)
}
func (o *SlogObserver) OnResolve(latency time.Duration, cached bool) {
	o.log().Debug("platcache route resolution", "latency_ms", float64(latency.Microseconds())/1000.0, "cached", cached)
}

// MetricsHook mirrors counter events into an external metrics system
// (e.g. telemetry.Metrics). Nil hooks are skipped.
type MetricsHook struct {
	OnHit        func(tenant, kind string)
	OnMiss       func(tenant string)
	OnStaleHit   func(tenant, kind string)
	OnRefresh    func(tenant, kind string, failed bool)
	OnInvalidate func(scope, reason string)
	OnDBFallback func()
	OnResolve    func(cached bool, seconds float64)
	OnLockWait   func(seconds float64)
}

func atomicAdd(ptr *int64, n int64) { atomic.AddInt64(ptr, n) }
