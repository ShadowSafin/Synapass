package platcache

import (
	"context"
	"time"
)

// HealthStatus is the cached dependency health assessment shared across
// replicas so they converge on one view without probing independently.
type HealthStatus struct {
	Dependency string    `json:"dependency"`
	Healthy    bool      `json:"healthy"`
	State      string    `json:"state"`
	LatencyMS  float64   `json:"latency_ms,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
}

// ReportHealth caches one dependency's health assessment.
func (c *Cache) ReportHealth(ctx context.Context, status HealthStatus) error {
	if c == nil || status.Dependency == "" {
		return nil
	}
	status.CheckedAt = time.Now()
	return c.Set(ctx, KindHealthStatus, HealthKey(status.Dependency), "", status,
		"kind:"+string(KindHealthStatus))
}

// CachedHealth reads the last assessment without triggering a probe.
func (c *Cache) CachedHealth(ctx context.Context, dependency string) (*HealthStatus, bool) {
	if c == nil {
		return nil, false
	}
	var out HealthStatus
	found, _, _, _ := c.Get(ctx, KindHealthStatus, HealthKey(dependency), "", &out)
	if !found {
		return nil, false
	}
	return &out, true
}

// HealthSnapshot returns cache health for dashboards: L1 size, L2
// reachability, hit rate, and per-family-relevant counters.
func (c *Cache) HealthSnapshot(ctx context.Context) map[string]any {
	out := map[string]any{
		"l1_entries": c.L1Len(),
		"hit_rate":   c.Stats().HitRate(),
	}
	s := c.Stats()
	out["hits"] = s.Hits
	out["misses"] = s.Misses
	out["stale_hits"] = s.StaleHits
	out["refreshes"] = s.Refreshes
	out["refresh_failures"] = s.RefreshFailures
	out["invalidations"] = s.Invalidations
	out["db_fallbacks"] = s.DBFallbacks
	out["coalesced"] = s.Coalesced
	out["store_errors"] = s.StoreErrors
	out["route_resolutions"] = s.RouteResolutions
	out["resolution_latency_ms"] = s.ResolutionLatencyMS
	if ra, ok := c.l2.(*RedisAdapter); ok && ra != nil {
		ok, latency, err := ra.Health(ctx)
		out["l2_ok"] = ok
		out["l2_latency_ms"] = float64(latency.Microseconds()) / 1000.0
		if err != nil {
			out["l2_error"] = err.Error()
		}
	} else if c.l2 == nil {
		out["l2_ok"] = false
		out["l2_mode"] = "absent (db-fallback)"
	} else {
		out["l2_ok"] = true
		out["l2_mode"] = "in-process"
	}
	return out
}
