package platcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Options configures the platform cache.
type Options struct {
	// L1MaxEntries bounds in-process memory.
	L1MaxEntries int
	// KeyPrefix namespaces every L2 key (e.g. "synapass").
	KeyPrefix string
	// CoalesceTimeout bounds how long rebuild waiters block.
	CoalesceTimeout time.Duration
	// LockTTL bounds distributed rebuild locks.
	LockTTL time.Duration
	// RefreshTimeout bounds background refresh loads.
	RefreshTimeout time.Duration
	// DisableL1 skips the memory layer (tests).
	DisableL1 bool
	// Logger for structured events. Nil uses slog.Default.
	Logger *slog.Logger
	// Observer for structured events. Nil uses SlogObserver.
	Observer Observer
	// Metrics mirrors events to Prometheus/telemetry. Nil skips.
	Metrics *MetricsHook
}

// DefaultOptions returns production-minded defaults.
func DefaultOptions() Options {
	return Options{
		L1MaxEntries:    4000,
		KeyPrefix:       "synapass",
		CoalesceTimeout: 3 * time.Second,
		LockTTL:         10 * time.Second,
		RefreshTimeout:  8 * time.Second,
	}
}

// Cache is the production multi-layer platform cache.
//
//	L1: *Memory (ultra-fast, ~2-10s TTL)
//	L2: Store  (Redis via adapter, or MapStore; primary, versioned envelopes)
//	L3: HTTP response helpers built on the same layers with strict safety
//
// All reads are tenant-aware where applicable, stampede-protected via
// single-flight + distributed locks, and observable via Stats/Observer.
type Cache struct {
	opts Options
	l1   *Memory
	l2   Store
	log  *slog.Logger

	flights *flightGroup

	mu    sync.Mutex
	tags  map[string]map[string]bool // tag -> keys
	stats Stats

	locksMu sync.Mutex
	locks   map[string]*heldLock
}

type heldLock struct {
	token   string
	expires time.Time
}

// New builds a Cache over an L2 store. A nil store means L1+DB-fallback
// only (Redis-down mode); the cache keeps serving via loader functions.
func New(l2 Store, opts Options) *Cache {
	if opts.KeyPrefix == "" {
		opts.KeyPrefix = "synapass"
	}
	if opts.CoalesceTimeout <= 0 {
		opts.CoalesceTimeout = 3 * time.Second
	}
	if opts.LockTTL <= 0 {
		opts.LockTTL = 10 * time.Second
	}
	if opts.RefreshTimeout <= 0 {
		opts.RefreshTimeout = 8 * time.Second
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	obs := opts.Observer
	if obs == nil {
		obs = &SlogObserver{Logger: log}
		opts.Observer = obs
	}
	var l1 *Memory
	if !opts.DisableL1 {
		l1 = NewMemory(opts.L1MaxEntries)
	}
	return &Cache{
		opts: opts, l1: l1, l2: l2, log: log,
		flights: newFlightGroup(opts.CoalesceTimeout),
		tags:    map[string]map[string]bool{},
		locks:   map[string]*heldLock{},
	}
}

// Stats returns a snapshot for dashboards.
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.stats
	return out
}

// L1Len returns the live L1 entry count.
func (c *Cache) L1Len() int {
	if c == nil || c.l1 == nil {
		return 0
	}
	return c.l1.Len()
}

func (c *Cache) observer() Observer {
	if c == nil || c.opts.Observer == nil {
		return &SlogObserver{}
	}
	return c.opts.Observer
}

func (c *Cache) namespaced(key string) string {
	if c.opts.KeyPrefix == "" {
		return "platcache:" + key
	}
	return c.opts.KeyPrefix + ":platcache:" + key
}

// ---------------------------------------------------------------------------
// Core typed API
// ---------------------------------------------------------------------------

// Get fetches a cached value into dst. It reports hit layer ("l1"/"l2"),
// staleness, and whether a background refresh was triggered.
// A miss returns found=false and no error (except programming errors);
// store outages are counted as misses with DB fallback expected by caller.
func (c *Cache) Get(ctx context.Context, kind Kind, key, tenant string, dst any) (found bool, stale bool, layer string, err error) {
	if c == nil {
		return false, false, "", nil
	}
	if verr := ValidateKey(kind, key, tenant); verr != nil {
		return false, false, "", verr
	}
	ns := c.namespaced(key)

	// L1 fast path: L1 holds raw envelopes; decode to honour soft/hard TTL
	// and tenant binding even for memory hits.
	if c.l1 != nil {
		if raw, ok := c.l1.Get(ns); ok {
			soft, hard, _, derr := decodeEnvelopeInto(raw, key, tenant, dst)
			if derr == nil && !hard {
				atomicAdd(&c.stats.Hits, 1)
				c.observer().OnHit(key, kind, tenant, "l1")
				if c.opts.Metrics != nil && c.opts.Metrics.OnHit != nil {
					c.opts.Metrics.OnHit(tenant, string(kind))
				}
				if soft {
					atomicAdd(&c.stats.StaleHits, 1)
					c.observer().OnStaleHit(key, kind, tenant)
				}
				return true, soft, "l1", nil
			}
			// Corrupt/expired L1 entry: drop and continue to L2.
			c.l1.Delete(ns)
		}
	}

	if c.l2 == nil {
		atomicAdd(&c.stats.Misses, 1)
		c.observer().OnMiss(key, kind, tenant, "no-l2")
		return false, false, "", nil
	}
	raw, ok, serr := c.l2.Get(ctx, ns)
	if serr != nil {
		atomicAdd(&c.stats.StoreErrors, 1)
		atomicAdd(&c.stats.Misses, 1)
		c.observer().OnFallback(key, serr)
		if c.opts.Metrics != nil && c.opts.Metrics.OnDBFallback != nil {
			c.opts.Metrics.OnDBFallback()
		}
		return false, false, "", nil
	}
	if !ok {
		atomicAdd(&c.stats.Misses, 1)
		c.observer().OnMiss(key, kind, tenant, "miss")
		if c.opts.Metrics != nil && c.opts.Metrics.OnMiss != nil {
			c.opts.Metrics.OnMiss(tenant)
		}
		return false, false, "", nil
	}
	soft, hard, _, derr := decodeEnvelopeInto(raw, key, tenant, dst)
	if derr != nil {
		// Corrupt entry: log, bypass for this request, evict lazily.
		c.log.Warn("platcache decode failed; bypassing", "key", key, "error", derr)
		atomicAdd(&c.stats.Misses, 1)
		return false, false, "", nil
	}
	if hard {
		atomicAdd(&c.stats.Misses, 1)
		c.observer().OnMiss(key, kind, tenant, "expired")
		return false, false, "", nil
	}
	// Populate L1 for the next hot hit.
	if c.l1 != nil {
		c.l1.Set(ns, raw, L1TTL(kind))
	}
	atomicAdd(&c.stats.Hits, 1)
	c.observer().OnHit(key, kind, tenant, "l2")
	if c.opts.Metrics != nil && c.opts.Metrics.OnHit != nil {
		c.opts.Metrics.OnHit(tenant, string(kind))
	}
	if soft {
		atomicAdd(&c.stats.StaleHits, 1)
		c.observer().OnStaleHit(key, kind, tenant)
		if c.opts.Metrics != nil && c.opts.Metrics.OnStaleHit != nil {
			c.opts.Metrics.OnStaleHit(tenant, string(kind))
		}
	}
	return true, soft, "l2", nil
}

// Set stores value under key with the kind's hard TTL (L2) and a short L1 TTL.
func (c *Cache) Set(ctx context.Context, kind Kind, key, tenant string, value any, tags ...string) error {
	if c == nil {
		return nil
	}
	if err := ValidateKey(kind, key, tenant); err != nil {
		return err
	}
	if err := AssertSafePayload(string(kind)); err != nil {
		return err
	}
	hard, soft := TTLFor(kind)
	return c.SetWithTTL(ctx, kind, key, tenant, value, hard, soft, tags...)
}

// SetWithTTL stores with explicit hard/soft TTLs.
func (c *Cache) SetWithTTL(ctx context.Context, kind Kind, key, tenant string, value any, hard, soft time.Duration, tags ...string) error {
	if c == nil {
		return nil
	}
	if err := ValidateKey(kind, key, tenant); err != nil {
		return err
	}
	if hard <= 0 {
		h, _ := TTLFor(kind)
		hard = h
	}
	if soft <= 0 || soft > hard {
		_, s := TTLFor(kind)
		if s > hard {
			s = hard / 2
		}
		soft = s
	}
	raw, err := encodeEnvelope(kind, key, tenant, value, hard, soft)
	if err != nil {
		c.log.Warn("platcache encode failed; bypassing write", "key", key, "error", err)
		return err
	}
	ns := c.namespaced(key)
	if c.l1 != nil {
		c.l1.Set(ns, raw, L1TTL(kind))
	}
	if c.l2 != nil {
		if serr := c.l2.Set(ctx, ns, raw, hard+30*time.Second); serr != nil {
			atomicAdd(&c.stats.StoreErrors, 1)
			c.log.Warn("platcache L2 write failed", "key", key, "error", serr)
			return nil // L1 still serves; never fail the request on cache write.
		}
	}
	if len(tags) > 0 {
		c.mu.Lock()
		for _, t := range tags {
			if c.tags[t] == nil {
				c.tags[t] = map[string]bool{}
			}
			c.tags[t][ns] = true
		}
		c.mu.Unlock()
	}
	// Index tenant tag implicitly so invalidateTenantCache needs no caller tags.
	if tenant != "" {
		c.mu.Lock()
		t := "tenant:" + normSegment(tenant)
		if c.tags[t] == nil {
			c.tags[t] = map[string]bool{}
		}
		c.tags[t][ns] = true
		c.mu.Unlock()
	}
	return nil
}

// GetOrSet is the primary read-through helper: cache first, single-flight
// DB loader on miss, then populate both layers.
//
// load must return a JSON-marshalable value and must never return secrets.
func (c *Cache) GetOrSet(ctx context.Context, kind Kind, key, tenant string, dst any, load func(ctx context.Context) (any, error), tags ...string) (hit bool, err error) {
	if c == nil {
		if load == nil {
			return false, nil
		}
		v, lerr := load(ctx)
		if lerr != nil {
			return false, lerr
		}
		if dst != nil && v != nil {
			raw, merr := json.Marshal(v)
			if merr == nil {
				_ = json.Unmarshal(raw, dst)
			}
		}
		return false, nil
	}
	found, stale, _, _ := c.Get(ctx, kind, key, tenant, dst)
	if found && !stale {
		return true, nil
	}
	if found && stale {
		// Stale-while-revalidate: serve stale now, refresh behind.
		c.RefreshInBackground(kind, key, tenant, load, tags...)
		return true, nil
	}
	if load == nil {
		return false, nil
	}
	raw, _, shared, lerr := c.flights.Do(key, func() ([]byte, time.Time, error) {
		v, err := load(withTenantCacheContext(ctx, tenant))
		if err != nil {
			return nil, time.Time{}, err
		}
		hard, soft := TTLFor(kind)
		env, err := encodeEnvelope(kind, key, tenant, v, hard, soft)
		if err != nil {
			return nil, time.Time{}, err
		}
		return env, time.Now(), nil
	})
	if lerr != nil {
		if errors.Is(lerr, errCoalesceTimeout) {
			atomicAdd(&c.stats.LockTimeouts, 1)
			// Coalesce timeout: load directly rather than pile up.
			atomicAdd(&c.stats.DBFallbacks, 1)
			v, err := load(withTenantCacheContext(ctx, tenant))
			if err != nil {
				return false, err
			}
			_ = c.Set(ctx, kind, key, tenant, v, tags...)
			if dst != nil && v != nil {
				cloneViaJSON(v, dst)
			}
			return false, nil
		}
		atomicAdd(&c.stats.DBFallbacks, 1)
		return false, lerr
	}
	if shared {
		atomicAdd(&c.stats.Coalesced, 1)
		c.observer().OnStampedeCoalesced(key)
	}
	// Publish the winning envelope to both layers.
	ns := c.namespaced(key)
	if c.l1 != nil {
		c.l1.Set(ns, raw, L1TTL(kind))
	}
	if c.l2 != nil {
		hard, _ := TTLFor(kind)
		if serr := c.l2.Set(ctx, ns, raw, hard+30*time.Second); serr != nil {
			atomicAdd(&c.stats.StoreErrors, 1)
		}
	}
	c.rememberTags(ns, tenant, tags)
	_, _, _, derr := decodeEnvelopeInto(raw, key, tenant, dst)
	if derr != nil {
		return false, nil
	}
	return false, nil
}

// GetOrSetStaleWhileRevalidate serves stale data for a short period while
// refreshing in the background; it never blocks the request path longer
// than necessary. On a cold miss it loads synchronously (single-flight).
func (c *Cache) GetOrSetStaleWhileRevalidate(ctx context.Context, kind Kind, key, tenant string, dst any, load func(ctx context.Context) (any, error), tags ...string) (hit, stale bool, err error) {
	if c == nil {
		if load == nil {
			return false, false, nil
		}
		v, lerr := load(ctx)
		if lerr != nil {
			return false, false, lerr
		}
		if dst != nil && v != nil {
			cloneViaJSON(v, dst)
		}
		return false, false, nil
	}
	found, isStale, _, _ := c.Get(ctx, kind, key, tenant, dst)
	if found {
		if isStale {
			c.RefreshInBackground(kind, key, tenant, load, tags...)
		}
		return true, isStale, nil
	}
	hit, lerr := c.GetOrSet(ctx, kind, key, tenant, dst, load, tags...)
	return hit, false, lerr
}

// RefreshInBackground reloads a hot key without blocking the request path.
// Failures keep serving stale data when safe and never break request flow.
func (c *Cache) RefreshInBackground(kind Kind, key, tenant string, load func(ctx context.Context) (any, error), tags ...string) {
	if c == nil || load == nil {
		return
	}
	if err := ValidateKey(kind, key, tenant); err != nil {
		return
	}
	atomicAdd(&c.stats.Refreshes, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), c.opts.RefreshTimeout)
		defer cancel()
		ctx = withTenantCacheContext(ctx, tenant)
		// Distributed guard: only one worker rebuilds expensive keys.
		locked, release := c.tryRebuildLock(ctx, "rebuild:"+key)
		if !locked {
			return
		}
		defer release()
		v, err := load(ctx)
		if err != nil {
			atomicAdd(&c.stats.RefreshFailures, 1)
			c.observer().OnRefresh(key, kind, tenant, err)
			if c.opts.Metrics != nil && c.opts.Metrics.OnRefresh != nil {
				c.opts.Metrics.OnRefresh(tenant, string(kind), true)
			}
			return
		}
		if serr := c.Set(ctx, kind, key, tenant, v, tags...); serr != nil {
			atomicAdd(&c.stats.RefreshFailures, 1)
			c.observer().OnRefresh(key, kind, tenant, serr)
			return
		}
		c.observer().OnRefresh(key, kind, tenant, nil)
		if c.opts.Metrics != nil && c.opts.Metrics.OnRefresh != nil {
			c.opts.Metrics.OnRefresh(tenant, string(kind), false)
		}
	}()
}

// ---------------------------------------------------------------------------
// Invalidation
// ---------------------------------------------------------------------------

// InvalidateByKey removes one logical key from L1+L2.
func (c *Cache) InvalidateByKey(ctx context.Context, key string) (int, error) {
	if c == nil {
		return 0, nil
	}
	ns := c.namespaced(key)
	removed := 0
	if c.l1 != nil {
		if _, ok := c.l1.Get(ns); ok {
			removed++
		}
		c.l1.Delete(ns)
	}
	if c.l2 != nil {
		if serr := c.l2.Delete(ctx, ns); serr != nil {
			atomicAdd(&c.stats.StoreErrors, 1)
			c.log.Warn("platcache L2 delete failed", "key", key, "error", serr)
		} else {
			removed++
		}
	}
	c.forgetKey(ns)
	atomicAdd(&c.stats.Invalidations, 1)
	atomicAdd(&c.stats.InvalidatedKeys, int64(removed))
	c.observer().OnInvalidate([]string{key}, nil, "by-key", removed)
	c.emitInvalidateMetric("key", "explicit")
	return removed, nil
}

// InvalidateByTag removes every key indexed under tag.
func (c *Cache) InvalidateByTag(ctx context.Context, tag string) (int, error) {
	if c == nil {
		return 0, nil
	}
	c.mu.Lock()
	keys := make([]string, 0, len(c.tags[tag]))
	for k := range c.tags[tag] {
		keys = append(keys, k)
	}
	delete(c.tags, tag)
	c.mu.Unlock()
	removed := 0
	for _, ns := range keys {
		if c.l1 != nil {
			c.l1.Delete(ns)
		}
		if c.l2 != nil {
			if serr := c.l2.Delete(ctx, ns); serr != nil {
				atomicAdd(&c.stats.StoreErrors, 1)
				continue
			}
		}
		removed++
		c.forgetKey(ns)
	}
	atomicAdd(&c.stats.Invalidations, 1)
	atomicAdd(&c.stats.InvalidatedKeys, int64(removed))
	c.observer().OnInvalidate(nil, []string{tag}, "by-tag", removed)
	c.emitInvalidateMetric("tag", "explicit")
	return removed, nil
}

// InvalidateByPrefix removes every namespaced key under prefix from L1+L2.
func (c *Cache) InvalidateByPrefix(ctx context.Context, prefix string) (int, error) {
	if c == nil {
		return 0, nil
	}
	nsPrefix := c.namespaced(prefix)
	removed := 0
	if c.l1 != nil {
		removed += c.l1.DeletePrefix(nsPrefix)
	}
	if c.l2 != nil {
		n, serr := c.l2.DeletePrefix(ctx, nsPrefix)
		if serr != nil {
			atomicAdd(&c.stats.StoreErrors, 1)
			c.log.Warn("platcache L2 prefix delete failed", "prefix", prefix, "error", serr)
		} else {
			removed += n
		}
	}
	atomicAdd(&c.stats.Invalidations, 1)
	atomicAdd(&c.stats.InvalidatedKeys, int64(removed))
	c.observer().OnInvalidate(nil, []string{"prefix:" + prefix}, "by-prefix", removed)
	return removed, nil
}

func (c *Cache) rememberTags(ns, tenant string, tags []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.tags == nil {
		c.tags = map[string]map[string]bool{}
	}
	for _, t := range tags {
		if c.tags[t] == nil {
			c.tags[t] = map[string]bool{}
		}
		c.tags[t][ns] = true
	}
	if tenant != "" {
		t := "tenant:" + normSegment(tenant)
		if c.tags[t] == nil {
			c.tags[t] = map[string]bool{}
		}
		c.tags[t][ns] = true
	}
}

func (c *Cache) forgetKey(ns string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for tag, set := range c.tags {
		delete(set, ns)
		if len(set) == 0 {
			delete(c.tags, tag)
		}
	}
}

func (c *Cache) emitInvalidateMetric(scope, reason string) {
	if c.opts.Metrics != nil && c.opts.Metrics.OnInvalidate != nil {
		c.opts.Metrics.OnInvalidate(scope, reason)
	}
}

// ---------------------------------------------------------------------------
// Distributed locks (cache rebuild guard)
// ---------------------------------------------------------------------------

// AcquireLock takes a named distributed lock. The Redis path uses SetNX;
// without L2 it falls back to an in-process mutex map so single-binary
// deployments still serialize rebuilds.
func (c *Cache) AcquireLock(ctx context.Context, name string, ttl time.Duration) (release func(), ok bool) {
	if c == nil {
		return func() {}, true
	}
	if ttl <= 0 {
		ttl = c.opts.LockTTL
	}
	start := time.Now()
	token := fmt.Sprintf("%d", time.Now().UnixNano())
	ns := c.namespaced("lock:" + normSegment(name))
	if c.l2 != nil {
		created, err := c.l2.SetNX(ctx, ns, []byte(token), ttl)
		if err != nil {
			atomicAdd(&c.stats.StoreErrors, 1)
			// Store error: allow the rebuild (fail-open) rather than stall traffic.
			return func() {}, true
		}
		if !created {
			atomicAdd(&c.stats.LockWaits, 1)
			c.emitLockWaitMetric(time.Since(start))
			return func() {}, false
		}
		return func() {
			_ = c.l2.Delete(context.Background(), ns)
		}, true
	}
	c.locksMu.Lock()
	defer c.locksMu.Unlock()
	if h, ok := c.locks[ns]; ok && time.Now().Before(h.expires) {
		atomicAdd(&c.stats.LockWaits, 1)
		return func() {}, false
	}
	c.locks[ns] = &heldLock{token: token, expires: time.Now().Add(ttl)}
	return func() {
		c.locksMu.Lock()
		defer c.locksMu.Unlock()
		if h, ok := c.locks[ns]; ok && h.token == token {
			delete(c.locks, ns)
		}
	}, true
}

// ReleaseLock is a no-op convenience for callers holding a release func;
// prefer the closure returned by AcquireLock.
func (c *Cache) ReleaseLock(release func()) {
	if release != nil {
		release()
	}
}

func (c *Cache) tryRebuildLock(ctx context.Context, name string) (bool, func()) {
	release, ok := c.AcquireLock(ctx, name, c.opts.LockTTL)
	if !ok {
		atomicAdd(&c.stats.LockWaits, 1)
	}
	return ok, release
}

func (c *Cache) emitLockWaitMetric(d time.Duration) {
	if c.opts.Metrics != nil && c.opts.Metrics.OnLockWait != nil {
		c.opts.Metrics.OnLockWait(d.Seconds())
	}
}

// ---------------------------------------------------------------------------
// Tenant context
// ---------------------------------------------------------------------------

type tenantCtxKey struct{}

// WithTenantCacheContext carries the tenant id for loader functions and
// logging so tenant isolation is explicit on every cache fill.
func WithTenantCacheContext(ctx context.Context, tenantID string) context.Context {
	return withTenantCacheContext(ctx, tenantID)
}

func withTenantCacheContext(ctx context.Context, tenantID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if tenantID == "" {
		return ctx
	}
	return context.WithValue(ctx, tenantCtxKey{}, tenantID)
}

// TenantFromCacheContext returns the tenant carried by WithTenantCacheContext.
func TenantFromCacheContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(tenantCtxKey{}).(string)
	return v
}

func cloneViaJSON(src, dst any) {
	if src == nil || dst == nil {
		return
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return
	}
	_ = json.Unmarshal(raw, dst)
}

// safeKeyError rewrites validation failures without echoing secrets.
func safeKeyError(kind Kind, err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "secret-like") {
		return fmt.Errorf("platcache: refusing to cache kind %q (safety policy)", string(kind))
	}
	return err
}

var _ = safeKeyError
