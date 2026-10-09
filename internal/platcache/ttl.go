package platcache

import "time"

// TTL bounds per data family. Every family has a hard TTL (absolute
// expiry) and a soft TTL (serve-stale + background refresh threshold).
// Values follow the task's TTL strategy table.
type ttlSpec struct {
	Hard time.Duration
	Soft time.Duration
}

// TTLFor returns the hard and soft TTL for a kind.
func TTLFor(kind Kind) (hard, soft time.Duration) {
	spec, ok := ttlTable[kind]
	if !ok {
		return 60 * time.Second, 30 * time.Second
	}
	return spec.Hard, spec.Soft
}

var ttlTable = map[Kind]ttlSpec{
	// tenant metadata: 60s to 300s
	KindTenantMeta:     {Hard: 120 * time.Second, Soft: 60 * time.Second},
	KindTenantGraph:    {Hard: 120 * time.Second, Soft: 60 * time.Second},
	KindWorkspaceMeta:  {Hard: 120 * time.Second, Soft: 60 * time.Second},
	KindTenantSettings: {Hard: 180 * time.Second, Soft: 90 * time.Second},
	// route resolution: 30s to 120s
	KindGatewayRoute:    {Hard: 60 * time.Second, Soft: 30 * time.Second},
	KindDashboardRoute:  {Hard: 60 * time.Second, Soft: 30 * time.Second},
	KindHostResolve:     {Hard: 60 * time.Second, Soft: 30 * time.Second},
	KindWorkspaceLookup: {Hard: 90 * time.Second, Soft: 45 * time.Second},
	// provider/model catalog: 5m to 30m
	KindProviderCaps:    {Hard: 15 * time.Minute, Soft: 5 * time.Minute},
	KindModelList:       {Hard: 15 * time.Minute, Soft: 5 * time.Minute},
	KindPricingSnapshot: {Hard: 30 * time.Minute, Soft: 10 * time.Minute},
	// feature flags: 30s to 5m
	KindFeatureFlag: {Hard: 120 * time.Second, Soft: 30 * time.Second},
	// aggregated usage stats: 10s to 60s
	KindUsageAggregate: {Hard: 30 * time.Second, Soft: 10 * time.Second},
	// health/dependency status: 5s to 30s
	KindHealthStatus: {Hard: 15 * time.Second, Soft: 5 * time.Second},
	// HTTP response cache: deliberately short.
	KindHTTPResponse: {Hard: 30 * time.Second, Soft: 15 * time.Second},
}

// L1TTL is the ultra-short in-process TTL cap. L1 never outlives a few
// seconds so a bad write converges almost immediately even if L2 is stale.
func L1TTL(kind Kind) time.Duration {
	_, soft := TTLFor(kind)
	l1 := soft / 6
	if l1 < 2*time.Second {
		l1 = 2 * time.Second
	}
	if l1 > 10*time.Second {
		l1 = 10 * time.Second
	}
	return l1
}
