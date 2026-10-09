package platcache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testCache(l2 Store) *Cache {
	opts := DefaultOptions()
	opts.CoalesceTimeout = 5 * time.Second
	opts.RefreshTimeout = 5 * time.Second
	return New(l2, opts)
}

func TestHitAndMiss(t *testing.T) {
	c := testCache(NewMapStore())
	ctx := context.Background()
	type meta struct {
		Name string `json:"name"`
		Plan string `json:"plan"`
	}
	var got meta
	found, _, _, err := c.Get(ctx, KindTenantMeta, TenantMetaKey("t1"), "t1", &got)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if found {
		t.Fatalf("expected miss on cold cache")
	}
	loads := 0
	hit, err := c.GetOrSet(ctx, KindTenantMeta, TenantMetaKey("t1"), "t1", &got, func(ctx context.Context) (any, error) {
		loads++
		return meta{Name: "Acme", Plan: "pro"}, nil
	})
	if err != nil {
		t.Fatalf("getorset: %v", err)
	}
	if hit {
		t.Fatalf("first fill must report miss")
	}
	if loads != 1 || got.Name != "Acme" {
		t.Fatalf("loader must run once and populate, got %+v loads=%d", got, loads)
	}
	// Second read: L1/L2 hit, loader must not run.
	var got2 meta
	hit, err = c.GetOrSet(ctx, KindTenantMeta, TenantMetaKey("t1"), "t1", &got2, func(ctx context.Context) (any, error) {
		loads++
		return meta{Name: "Other"}, nil
	})
	if err != nil {
		t.Fatalf("second getorset: %v", err)
	}
	if !hit {
		t.Fatalf("expected hit on warm cache")
	}
	if loads != 1 {
		t.Fatalf("loader ran on hit (loads=%d)", loads)
	}
	if got2.Name != "Acme" {
		t.Fatalf("hit served wrong value: %+v", got2)
	}
	s := c.Stats()
	if s.Hits == 0 || s.Misses == 0 {
		t.Fatalf("stats must record hits and misses: %+v", s)
	}
}

func TestInvalidationOnWrites(t *testing.T) {
	c := testCache(NewMapStore())
	ctx := context.Background()
	var v map[string]string
	if _, err := c.GetOrSet(ctx, KindTenantMeta, TenantMetaKey("t9"), "t9", &v, func(ctx context.Context) (any, error) {
		return map[string]string{"a": "1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetOrSet(ctx, KindTenantSettings, TenantSettingsKey("t9"), "t9", &v, func(ctx context.Context) (any, error) {
		return map[string]string{"s": "1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InvalidateTenantCache(ctx, "t9"); err != nil {
		t.Fatal(err)
	}
	var check map[string]string
	found, _, _, _ := c.Get(ctx, KindTenantMeta, TenantMetaKey("t9"), "t9", &check)
	if found {
		t.Fatalf("tenant meta must miss after InvalidateTenantCache")
	}
	found, _, _, _ = c.Get(ctx, KindTenantSettings, TenantSettingsKey("t9"), "t9", &check)
	if found {
		t.Fatalf("tenant settings must miss after InvalidateTenantCache")
	}

	// Catalog invalidation.
	var cat []string
	if _, _, err := c.GetOrSetStaleWhileRevalidate(ctx, KindModelList, CatalogModelsKey(), "", &cat, func(ctx context.Context) (any, error) {
		return []string{"m1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InvalidateCatalogCache(ctx); err != nil {
		t.Fatal(err)
	}
	var cat2 []string
	found, _, _, _ = c.Get(ctx, KindModelList, CatalogModelsKey(), "", &cat2)
	if found {
		t.Fatalf("catalog must miss after InvalidateCatalogCache")
	}

	// Provider invalidation retires catalog too.
	if _, _, err := c.GetOrSetStaleWhileRevalidate(ctx, KindModelList, CatalogProvidersKey(), "", &cat, func(ctx context.Context) (any, error) {
		return []string{"p1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InvalidateProviderCache(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	found, _, _, _ = c.Get(ctx, KindModelList, CatalogProvidersKey(), "", &cat2)
	if found {
		t.Fatalf("providers must miss after InvalidateProviderCache")
	}

	// Route invalidation.
	res, _, err := c.ResolveTenantByHostPath(ctx, "acme.example.com", "/chat", func(ctx context.Context, host, path string) (*TenantResolution, error) {
		return &TenantResolution{TenantID: "t9"}, nil
	})
	if err != nil || res.TenantID != "t9" {
		t.Fatalf("resolve: %+v %v", res, err)
	}
	if _, err := c.InvalidateRouteCache(ctx, "acme.example.com", "/chat"); err != nil {
		t.Fatal(err)
	}
	loads := 0
	res2, cached, err := c.ResolveTenantByHostPath(ctx, "acme.example.com", "/chat", func(ctx context.Context, host, path string) (*TenantResolution, error) {
		loads++
		return &TenantResolution{TenantID: "t9"}, nil
	})
	if err != nil || cached || loads != 1 || res2.TenantID != "t9" {
		t.Fatalf("route must reload after invalidate: cached=%v loads=%d err=%v", cached, loads, err)
	}
}

func TestTenantIsolation(t *testing.T) {
	c := testCache(NewMapStore())
	ctx := context.Background()
	var a, b map[string]string
	if _, err := c.GetOrSet(ctx, KindTenantMeta, TenantMetaKey("tenant-a"), "tenant-a", &a, func(ctx context.Context) (any, error) {
		return map[string]string{"who": "a"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetOrSet(ctx, KindTenantMeta, TenantMetaKey("tenant-b"), "tenant-b", &b, func(ctx context.Context) (any, error) {
		return map[string]string{"who": "b"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if a["who"] != "a" || b["who"] != "b" {
		t.Fatalf("cross-tenant leakage: a=%v b=%v", a, b)
	}
	// Flushing A must not touch B.
	if _, err := c.InvalidateTenantCache(ctx, "tenant-a"); err != nil {
		t.Fatal(err)
	}
	var b2 map[string]string
	found, _, _, _ := c.Get(ctx, KindTenantMeta, TenantMetaKey("tenant-b"), "tenant-b", &b2)
	if !found || b2["who"] != "b" {
		t.Fatalf("tenant-b must survive tenant-a flush: found=%v %v", found, b2)
	}
	// Tenant-scoped keys require a tenant id.
	if err := ValidateKey(KindTenantMeta, TenantMetaKey("x"), ""); err == nil {
		t.Fatalf("tenant-scoped kind without tenant must fail validation")
	}
	// A key that does not embed its tenant must fail validation.
	if err := ValidateKey(KindTenantMeta, TenantMetaKey("tenant-a"), "tenant-b"); err == nil {
		t.Fatalf("key/tenant mismatch must fail validation")
	}
}

func TestSecretsNeverCached(t *testing.T) {
	cases := []struct {
		kind Kind
		key  string
	}{
		{KindTenantMeta, "tenant:t1:api_key:v1"},
		{KindTenantMeta, "tenant:t1:auth_token:v1"},
		{KindTenantMeta, "tenant:t1:session_secret:v1"},
		{KindTenantMeta, "tenant:t1:password:v1"},
		{KindTenantMeta, "tenant:t1:payment:v1"},
		{KindTenantMeta, "tenant:t1:credential:v1"},
		{KindTenantMeta, "tenant:t1:private_key:v1"},
		{KindTenantMeta, "tenant:t1:webhook_payload:v1"},
		{KindTenantMeta, "tenant:t1:raw_body:v1"},
	}
	for _, tc := range cases {
		if err := ValidateKey(tc.kind, tc.key, "t1"); err == nil {
			t.Fatalf("secret-like key %q must be refused", tc.key)
		}
	}
	if err := AssertSafePayload("raw_body"); err == nil {
		t.Fatalf("raw_body family must be refused")
	}
	c := testCache(NewMapStore())
	ctx := context.Background()
	var dst map[string]string
	if err := c.Set(ctx, KindTenantMeta, "tenant:t1:api_key:v1", "t1", map[string]string{"x": "y"}); err == nil {
		t.Fatalf("Set must refuse secret-like keys")
	}
	_ = dst
}

func TestStaleWhileRevalidate(t *testing.T) {
	c := testCache(NewMapStore())
	ctx := context.Background()
	key := TenantMetaKey("swr-t")
	// Seed with an already-soft-stale entry: hard 300ms, soft 50ms.
	if err := c.SetWithTTL(ctx, KindTenantMeta, key, "swr-t", map[string]string{"v": "old"},
		300*time.Millisecond, 50*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	var got map[string]string
	hit, stale, err := c.GetOrSetStaleWhileRevalidate(ctx, KindTenantMeta, key, "swr-t", &got,
		func(ctx context.Context) (any, error) {
			return map[string]string{"v": "new"}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if !hit || !stale {
		t.Fatalf("must serve stale while revalidating: hit=%v stale=%v", hit, stale)
	}
	if got["v"] != "old" {
		t.Fatalf("stale read must serve old value, got %v", got)
	}
	// Background refresh converges to the new value.
	deadline := time.Now().Add(3 * time.Second)
	for {
		var cur map[string]string
		found, s, _, _ := c.Get(ctx, KindTenantMeta, key, "swr-t", &cur)
		if found && !s && cur["v"] == "new" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background refresh did not converge (found=%v stale=%v val=%v)", found, s, cur)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := c.Stats(); got.StaleHits == 0 || got.Refreshes == 0 {
		t.Fatalf("stats must record stale hit + refresh: %+v", got)
	}
}

func TestRedisDownFallback(t *testing.T) {
	c := testCache(&FailingStore{})
	ctx := context.Background()
	var got map[string]string
	loads := 0
	hit, err := c.GetOrSet(ctx, KindTenantMeta, TenantMetaKey("down-t"), "down-t", &got, func(ctx context.Context) (any, error) {
		loads++
		return map[string]string{"v": "db"}, nil
	})
	if err != nil {
		t.Fatalf("Redis-down read must fall back to DB, got %v", err)
	}
	if hit {
		t.Fatalf("fallback load must report miss")
	}
	if loads != 1 || got["v"] != "db" {
		t.Fatalf("fallback must serve DB value: %+v loads=%d", got, loads)
	}
	// Refresh failures keep serving stale-safe behaviour: request still works.
	c.RefreshInBackground(KindTenantMeta, TenantMetaKey("down-t"), "down-t", func(ctx context.Context) (any, error) {
		return nil, errors.New("db down too")
	})
	time.Sleep(100 * time.Millisecond) // let the goroutine finish without flake
	s := c.Stats()
	if s.StoreErrors == 0 {
		t.Fatalf("store errors must be counted: %+v", s)
	}
}

func TestStampedeProtection(t *testing.T) {
	c := testCache(NewMapStore())
	ctx := context.Background()
	key := TenantMetaKey("hot-t")
	var calls int64
	load := func(ctx context.Context) (any, error) {
		atomic.AddInt64(&calls, 1)
		time.Sleep(150 * time.Millisecond) // expensive rebuild
		return map[string]string{"v": "hot"}, nil
	}
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var dst map[string]string
			_, err := c.GetOrSet(ctx, KindTenantMeta, key, "hot-t", &dst, load)
			errs[i] = err
			if dst["v"] != "hot" {
				errs[i] = fmt.Errorf("wrong value %v", dst)
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Fatalf("single-flight must rebuild once, rebuilt %d times", calls)
	}
	if got := c.Stats(); got.Coalesced == 0 {
		t.Fatalf("coalesced rebuilds must be counted: %+v", got)
	}
	// Distributed lock serializes rebuilds: second holder loses.
	release, ok := c.AcquireLock(ctx, "rebuild:test", time.Minute)
	if !ok {
		t.Fatalf("first lock must succeed")
	}
	_, ok2 := c.AcquireLock(ctx, "rebuild:test", time.Minute)
	if ok2 {
		t.Fatalf("second lock must lose while held")
	}
	c.ReleaseLock(release)
	if got := c.Stats(); got.LockWaits == 0 {
		t.Fatalf("lock contention must be counted: %+v", got)
	}
}

func TestPrewarmAfterProvisioning(t *testing.T) {
	c := testCache(NewMapStore())
	ctx := context.Background()
	if err := c.PrewarmTenant(ctx, "new-t", TenantPrewarmLoads{
		Meta: func(ctx context.Context) (any, error) {
			return map[string]string{"id": "new-t", "plan": "pro"}, nil
		},
		Settings: func(ctx context.Context) (any, error) {
			return map[string]string{"theme": "dark"}, nil
		},
		Graph: func(ctx context.Context) (any, error) {
			return map[string]string{"route": "openai"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	found, _, _, _ := c.Get(ctx, KindTenantMeta, TenantMetaKey("new-t"), "new-t", &meta)
	if !found || meta["id"] != "new-t" {
		t.Fatalf("prewarmed meta must hit: %v %+v", found, meta)
	}
	if err := c.PrewarmCatalog(ctx, []string{"p1"}, []string{"m1"}, map[string]float64{"m1": 1.5}); err != nil {
		t.Fatal(err)
	}
	var models []string
	found, _, _, _ = c.Get(ctx, KindModelList, CatalogModelsKey(), "", &models)
	if !found || len(models) != 1 || models[0] != "m1" {
		t.Fatalf("prewarmed catalog must hit: %v %+v", found, models)
	}
	if err := c.PrewarmFeatureFlags(ctx, map[string]bool{"x": true}); err != nil {
		t.Fatal(err)
	}
	var flags map[string]bool
	found, _, _, _ = c.Get(ctx, KindFeatureFlag, FeatureFlagsGlobalKey(), "", &flags)
	if !found || !flags["x"] {
		t.Fatalf("prewarmed flags must hit: %v %+v", found, flags)
	}
	if got := c.Stats(); got.Prewarms == 0 {
		t.Fatalf("prewarms must be counted: %+v", got)
	}
}

func TestRouteResolutionCorrectness(t *testing.T) {
	c := testCache(NewMapStore())
	ctx := context.Background()
	loads := 0
	load := func(ctx context.Context, host, path string) (*TenantResolution, error) {
		loads++
		if host == "a.example.com" {
			return &TenantResolution{TenantID: "ta"}, nil
		}
		return &TenantResolution{TenantID: "tb"}, nil
	}
	ra, cached, err := c.ResolveTenantByHostPath(ctx, "a.example.com", "/chat", load)
	if err != nil || cached || ra.TenantID != "ta" {
		t.Fatalf("first resolve must miss and load: %+v cached=%v err=%v", ra, cached, err)
	}
	ra2, cached2, err := c.ResolveTenantByHostPath(ctx, "a.example.com", "/chat", load)
	if err != nil || !cached2 || ra2.TenantID != "ta" {
		t.Fatalf("second resolve must hit: %+v cached=%v err=%v", ra2, cached2, err)
	}
	rb, _, err := c.ResolveTenantByHostPath(ctx, "b.example.com", "/chat", load)
	if err != nil || rb.TenantID != "tb" {
		t.Fatalf("different host must resolve independently: %+v %v", rb, err)
	}
	if loads != 2 {
		t.Fatalf("each distinct route must load once, loads=%d", loads)
	}
	// Workspace slug mapping.
	w, _, err := c.ResolveTenantByWorkspaceSlug(ctx, "acme", func(ctx context.Context, slug string) (*WorkspaceLookup, error) {
		return &WorkspaceLookup{WorkspaceID: "w1", TenantID: "ta", Slug: slug}, nil
	})
	if err != nil || w.TenantID != "ta" {
		t.Fatalf("workspace resolve: %+v %v", w, err)
	}
	s := c.Stats()
	if s.RouteResolutions == 0 {
		t.Fatalf("route resolutions must be counted: %+v", s)
	}
}

func TestKeyShapes(t *testing.T) {
	if got := TenantMetaKey("T1"); got != "tenant:t1:meta:v1" {
		t.Fatalf("tenant meta key: %q", got)
	}
	if got := TenantRoutesKey("T1"); got != "tenant:t1:routes:v1" {
		t.Fatalf("tenant routes key: %q", got)
	}
	if got := WorkspaceTenantKey("W1"); got != "workspace:w1:tenant:v1" {
		t.Fatalf("workspace key: %q", got)
	}
	if got := GatewayResolveKey("Example.COM", "/Chat"); got != "gateway:resolve:example.com:chat:v1" {
		t.Fatalf("gateway key: %q", got)
	}
	if CatalogProvidersKey() != "modelcatalog:providers:v3" {
		t.Fatalf("catalog providers key: %q", CatalogProvidersKey())
	}
	if CatalogPricingKey() != "modelcatalog:pricing:v3" {
		t.Fatalf("catalog pricing key: %q", CatalogPricingKey())
	}
	if FeatureFlagsGlobalKey() != "featureflags:global:v2" {
		t.Fatalf("flags key: %q", FeatureFlagsGlobalKey())
	}
}

func TestHTTPResponseCacheSafety(t *testing.T) {
	// Sensitive paths never qualify.
	for _, p := range []string{"/admin/v1/tenants", "/admin/v1/auth/login", "/v1/feedback", "/admin/v1/keys"} {
		if SafeGET("GET", p, DefaultPublicGETPrefixes()) {
			t.Fatalf("sensitive path %q must not be cacheable", p)
		}
	}
	if !SafeGET("GET", "/version", DefaultPublicGETPrefixes()) {
		t.Fatalf("/version must be cacheable")
	}
	if SafeGET("POST", "/version", DefaultPublicGETPrefixes()) {
		t.Fatalf("only GET may be cached")
	}
	c := testCache(NewMapStore())
	ctx := context.Background()
	body := []byte(`{"version":"1.0"}`)
	if err := c.SetHTTPResponse(ctx, "GET", "/version", "", "public", body, 0); err != nil {
		t.Fatal(err)
	}
	got, ok := c.GetHTTPResponse(ctx, "public", HashGETPath("GET", "/version", ""))
	if !ok || string(got) != string(body) {
		t.Fatalf("http cache must hit: ok=%v got=%q", ok, got)
	}
	// Auth endpoints are never stored even if forced.
	if err := c.SetHTTPResponse(ctx, "GET", "/admin/v1/auth/login", "", "public", body, 0); err != nil {
		t.Fatal(err)
	}
	_, ok = c.GetHTTPResponse(ctx, "public", HashGETPath("GET", "/admin/v1/auth/login", ""))
	if ok {
		t.Fatalf("sensitive GET must never be cached")
	}
}

func TestTenantContext(t *testing.T) {
	ctx := WithTenantCacheContext(context.Background(), "t-ctx")
	if got := TenantFromCacheContext(ctx); got != "t-ctx" {
		t.Fatalf("tenant context: %q", got)
	}
	if got := TenantFromCacheContext(context.Background()); got != "" {
		t.Fatalf("empty context must yield empty tenant: %q", got)
	}
}

func TestHealthAndGraphInvalidation(t *testing.T) {
	c := testCache(NewMapStore())
	ctx := context.Background()
	if err := c.ReportHealth(ctx, HealthStatus{Dependency: "openai", Healthy: true, State: "healthy"}); err != nil {
		t.Fatal(err)
	}
	h, ok := c.CachedHealth(ctx, "openai")
	if !ok || !h.Healthy {
		t.Fatalf("cached health must hit: %+v %v", h, ok)
	}
	var g map[string]string
	if _, err := c.GetOrSet(ctx, KindTenantGraph, TenantGraphKey("g1"), "g1", &g, func(ctx context.Context) (any, error) {
		return map[string]string{"r": "1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InvalidateTenantGraphCache(ctx, "g1"); err != nil {
		t.Fatal(err)
	}
	var g2 map[string]string
	found, _, _, _ := c.Get(ctx, KindTenantGraph, TenantGraphKey("g1"), "g1", &g2)
	if found {
		t.Fatalf("graph must miss after InvalidateTenantGraphCache")
	}
	snap := c.HealthSnapshot(ctx)
	if snap["enabled"] != nil {
		t.Fatalf("bare snapshot must not carry api enabled flag")
	}
	if snap["hits"] == nil || snap["l1_entries"] == nil {
		t.Fatalf("snapshot must carry counters: %v", snap)
	}
}

func TestCorruptEntryBypass(t *testing.T) {
	store := NewMapStore()
	c := testCache(store)
	ctx := context.Background()
	ns := c.namespaced(TenantMetaKey("cz"))
	_ = store.Set(ctx, ns, []byte("{not-json"), time.Minute)
	var dst map[string]string
	found, _, _, err := c.Get(ctx, KindTenantMeta, TenantMetaKey("cz"), "cz", &dst)
	if err != nil {
		t.Fatalf("corrupt entry must bypass without error, got %v", err)
	}
	if found {
		t.Fatalf("corrupt entry must miss")
	}
}
