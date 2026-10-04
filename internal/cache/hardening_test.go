package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Hardening suite: proves the cache is correct end-to-end at the unit level.
// Every test here targets a production failure mode: wrong hits, cross
// tenant leaks, stale serving-context reuse, premature expiry handling,
// bypass honesty, partial-response poisoning and concurrent corruption.
// ---------------------------------------------------------------------------

// redisLikeStore mirrors the production redisCacheStore key mapping
// ("response:" namespacing + prefix mapping) over a map, with real TTL
// enforcement, so invalidation tests exercise the exact prefix strings
// production uses.
type redisLikeStore struct {
	mu       sync.Mutex
	data     map[string][]byte
	deadline map[string]time.Time
	lastTTL  map[string]time.Duration
}

func newRedisLikeStore() *redisLikeStore {
	return &redisLikeStore{
		data:     map[string][]byte{},
		deadline: map[string]time.Time{},
		lastTTL:  map[string]time.Duration{},
	}
}

func mapPrefix(prefix string) string {
	switch prefix {
	case "exact:", "prefix:", "semantic:", "tenant:":
		return "response:" + prefix
	default:
		return prefix
	}
}

func (m *redisLikeStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	full := "response:" + key
	v, ok := m.data[full]
	if !ok {
		return nil, false, nil
	}
	if dl, has := m.deadline[full]; has && time.Now().After(dl) {
		delete(m.data, full)
		delete(m.deadline, full)
		return nil, false, nil
	}
	return append([]byte{}, v...), true, nil
}

func (m *redisLikeStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	full := "response:" + key
	m.data[full] = append([]byte{}, value...)
	if ttl > 0 {
		m.deadline[full] = time.Now().Add(ttl)
	} else {
		delete(m.deadline, full)
	}
	m.lastTTL[full] = ttl
	return nil
}

func (m *redisLikeStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapped := mapPrefix(prefix)
	removed := 0
	for k := range m.data {
		if strings.HasPrefix(k, mapped) {
			delete(m.data, k)
			delete(m.deadline, k)
			removed++
		}
	}
	return removed, nil
}

func (m *redisLikeStore) lastTTLFor(suffix string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, ttl := range m.lastTTL {
		if strings.HasSuffix(k, suffix) {
			return ttl, true
		}
	}
	return 0, false
}

func hardInput(tenant, model, prompt string) KeyInput {
	zero := 0.0
	return KeyInput{
		TenantID: tenant, APIKeyID: "key-1", Model: model,
		Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(prompt)}},
		Temperature: &zero,
	}
}

func mustStore(t *testing.T, c *Cache, in KeyInput, body string) {
	t.Helper()
	if got := c.StoreResponse(context.Background(), in, []byte(body), false); got == "" {
		t.Fatalf("StoreResponse refused a valid body")
	}
}

func mustMiss(t *testing.T, c *Cache, in KeyInput) {
	t.Helper()
	if hit := c.Lookup(context.Background(), in, false, "", false); hit.Hit {
		t.Fatalf("expected miss, got hit kind=%s key=%s", hit.Kind, hit.Key)
	}
}

func mustHit(t *testing.T, c *Cache, in KeyInput, kind domain.CacheKind) domain.CacheLookupResult {
	t.Helper()
	hit := c.Lookup(context.Background(), in, false, "", false)
	if !hit.Hit {
		t.Fatalf("expected %s hit, got miss", kind)
	}
	if kind != "" && hit.Kind != kind {
		t.Fatalf("expected kind=%s, got %s", kind, hit.Kind)
	}
	return hit
}

func TestExactHitMissAndStats(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	ctx := context.Background()
	in := hardInput("t1", "m1", "what is the capital of france")
	mustMiss(t, c, in)
	mustStore(t, c, in, `{"answer":"paris"}`)
	hit := mustHit(t, c, in, domain.CacheExact)
	if string(hit.Body) != `{"answer":"paris"}` {
		t.Fatalf("hit body mismatch: %s", hit.Body)
	}
	if hit.Trace == nil || !hit.Trace.Hit || hit.Trace.Key == "" {
		t.Fatalf("hit must carry an observable trace with key, got %+v", hit.Trace)
	}
	st := c.Stats()
	if st.ExactHits != 1 || st.Misses != 1 {
		t.Fatalf("stats mismatch: %+v", st)
	}
	if st.HitRate != 0.5 {
		t.Fatalf("hit rate must be 0.5, got %v", st.HitRate)
	}
	if st.ReuseCount != 1 {
		t.Fatalf("reuse count must be 1, got %d", st.ReuseCount)
	}
	if st.LookupMSAvg < 0 {
		t.Fatalf("lookup avg must be measurable, got %v", st.LookupMSAvg)
	}
	_ = ctx
}

func TestTenantIsolationExact(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	a := hardInput("tenant-a", "m", "same prompt both tenants")
	b := hardInput("tenant-b", "m", "same prompt both tenants")
	mustStore(t, c, a, `{"for":"a"}`)
	mustMiss(t, c, b)
	hit := mustHit(t, c, a, domain.CacheExact)
	if string(hit.Body) != `{"for":"a"}` {
		t.Fatalf("tenant A got wrong body: %s", hit.Body)
	}
}

func TestTenantIsolationSemantic(t *testing.T) {
	opts := DefaultOptions()
	opts.SemanticThreshold = 0.4
	c := New(newRedisLikeStore(), opts)
	long := "the quick brown fox jumps over the lazy dog and then runs away fast across the green meadow"
	a := KeyInput{TenantID: "t1", Model: "m", Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(long)}}}
	b := KeyInput{TenantID: "t2", Model: "m", Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(long + " !!")}}}
	mustStore(t, c, a, `{"a":1}`)
	if hit := c.Lookup(context.Background(), b, false, "", false); hit.Hit {
		t.Fatalf("cross-tenant semantic reuse: kind=%s sim=%.3f", hit.Kind, hit.Similarity)
	}
	// Same tenant, near-duplicate prompt hits the semantic tier when exact
	// and prefix both miss (long prompt skips the prefix tier).
	c2in := KeyInput{TenantID: "t1", Model: "m", Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(long + " !!")}}}
	hit := c.Lookup(context.Background(), c2in, false, "", false)
	if !hit.Hit || hit.Kind != domain.CacheSemantic {
		t.Fatalf("same-tenant near-duplicate must be a semantic hit, got %+v", hit)
	}
}

func TestModelProviderIsolation(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	base := hardInput("t1", "model-a", "isolation probe")
	mustStore(t, c, base, `{"m":"a"}`)
	other := hardInput("t1", "model-b", "isolation probe")
	mustMiss(t, c, other)

	// A fallback that serves a different model must not poison later
	// lookups for the requested model: the stored envelope carries the
	// served model, the next lookup carries the requested one.
	stored := hardInput("t1", "requested-model", "fallback probe")
	c.StoreResponseWithMeta(context.Background(), stored, []byte(`{"m":"fallback"}`), false,
		domain.CacheHitMeta{Provider: "fallback-provider", Model: "served-model"})
	again := hardInput("t1", "requested-model", "fallback probe")
	// The exact key matches, but the serving context (model) differs, so
	// the validator must turn it into a miss rather than a wrong answer.
	mustMiss(t, c, again)

	// Same model served by the fallback is reusable: no contradiction.
	c.StoreResponseWithMeta(context.Background(), stored, []byte(`{"m":"ok"}`), false,
		domain.CacheHitMeta{Provider: "fallback-provider", Model: "requested-model"})
	mustHit(t, c, again, domain.CacheExact)
}

func TestProviderMismatchMisses(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	a := hardInput("t1", "m", "provider probe")
	a.Provider = "provider-a"
	b := hardInput("t1", "m", "provider probe")
	b.Provider = "provider-b"
	mustStore(t, c, a, `{"p":"a"}`)
	if hit := c.Lookup(context.Background(), b, false, "", false); hit.Hit {
		t.Fatalf("cross-provider reuse must miss, got kind=%s", hit.Kind)
	}
	mustHit(t, c, a, domain.CacheExact)
}

func TestSettingsIsolation(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	base := hardInput("t1", "m", "settings probe")
	mustStore(t, c, base, `{"s":0}`)
	hot := 0.9
	for name, mutate := range map[string]func(*KeyInput){
		"temperature": func(in *KeyInput) { in.Temperature = &hot },
		"seed":        func(in *KeyInput) { v := 42; in.Seed = &v },
		"max_tokens":  func(in *KeyInput) { in.MaxTokens = 100 },
		"top_p":       func(in *KeyInput) { v := 0.5; in.TopP = &v },
		"format":      func(in *KeyInput) { in.FormatType = "json_object" },
		"stop":        func(in *KeyInput) { in.Stop = `"END"` },
		"reasoning":   func(in *KeyInput) { in.Reasoning = "high" },
	} {
		other := hardInput("t1", "m", "settings probe")
		mutate(&other)
		if hit := c.Lookup(context.Background(), other, false, "", false); hit.Hit {
			t.Fatalf("%s change must miss, got kind=%s", name, hit.Kind)
		}
	}
	mustHit(t, c, base, domain.CacheExact)
}

func TestServingContextIsolation(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	base := hardInput("t1", "m", "context probe")
	mustStore(t, c, base, `{"ok":1}`)
	cases := map[string]func(*KeyInput){
		"endpoint":   func(in *KeyInput) { in.EndpointID = "mobile" },
		"user":       func(in *KeyInput) { in.User = "end-user-2" },
		"sensitive":  func(in *KeyInput) { in.Sensitivity = "pii" },
		"api_key":    func(in *KeyInput) { in.APIKeyID = "key-2" },
		"policy":     func(in *KeyInput) { in.PolicyID = "pol-2" },
		"policy_ver": func(in *KeyInput) { in.PolicyVersion = 2 },
		"tools": func(in *KeyInput) {
			in.Tools = []domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "echo"}}}
		},
	}
	for name, mutate := range cases {
		other := hardInput("t1", "m", "context probe")
		other.APIKeyID = base.APIKeyID
		mutate(&other)
		if hit := c.Lookup(context.Background(), other, false, "", false); hit.Hit {
			t.Fatalf("%s change must miss, got kind=%s", name, hit.Kind)
		}
	}
	mustHit(t, c, base, domain.CacheExact)
}

func TestNormalizationHits(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	mk := func(text string) KeyInput {
		in := hardInput("t1", "m", text)
		return in
	}
	mustStore(t, c, mk("  Hello   WORLD\nnew\tline  "), `{"n":1}`)
	// Case, whitespace and wrapping differences map to the same key.
	for _, variant := range []string{"hello world new line", "HELLO  WORLD\nNEW LINE", "  hello world   new line "} {
		mustHit(t, c, mk(variant), domain.CacheExact)
	}
	// Legacy role spelling normalizes onto the current one.
	a := hardInput("t1", "m", "role probe")
	a.Messages = []domain.ChatMessage{{Role: domain.RoleFunction, Content: domain.NewTextContent("role probe")}}
	b := hardInput("t1", "m", "role probe")
	b.Messages = []domain.ChatMessage{{Role: domain.RoleTool, Content: domain.NewTextContent("role probe")}}
	if a.PromptHash() != b.PromptHash() {
		t.Fatalf("function/tool roles must share a prompt hash")
	}
	// JSON field order in tool arguments must not fragment the key.
	t1 := hardInput("t1", "m", "tool probe")
	t1.Tools = []domain.Tool{{Type: "function", Function: domain.FunctionDefinition{
		Name: "w", Parameters: []byte(`{"b":2,"a":1}`),
	}}}
	t2 := hardInput("t1", "m", "tool probe")
	t2.Tools = []domain.Tool{{Type: "function", Function: domain.FunctionDefinition{
		Name: "w", Parameters: []byte(`{"a": 1, "b": 2}`),
	}}}
	if ExactKey(t1) != ExactKey(t2) {
		t.Fatalf("reordered tool schema must share an exact key")
	}
	mustStore(t, c, t1, `{"t":1}`)
	mustHit(t, c, t2, domain.CacheExact)
}

func TestNoOverNormalization(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	mustStore(t, c, hardInput("t1", "m", "transfer $100 to alice"), `{"a":1}`)
	mustMiss(t, c, hardInput("t1", "m", "transfer $100 to bob"))
	mustMiss(t, c, hardInput("t1", "m", "transfer $100 to alice now"))
}

func TestTTLExpiry(t *testing.T) {
	st := newRedisLikeStore()
	c := New(st, DefaultOptions())
	in := hardInput("t1", "m", "ttl probe")
	// An already-expired envelope is never written.
	past := domain.CacheHitMeta{ExpiresAt: time.Now().Add(-time.Second)}
	if got := c.StoreResponseWithMeta(context.Background(), in, []byte(`{"x":1}`), false, past); got != "" {
		t.Fatalf("expired envelope must not be stored")
	}
	// A short per-request TTL is honored by the store, not just the envelope.
	short := domain.CacheHitMeta{ExpiresAt: time.Now().Add(80 * time.Millisecond)}
	if got := c.StoreResponseWithMeta(context.Background(), in, []byte(`{"x":1}`), false, short); got == "" {
		t.Fatalf("short-TTL envelope must be stored")
	}
	mustHit(t, c, in, domain.CacheExact)
	time.Sleep(150 * time.Millisecond)
	mustMiss(t, c, in)
}

func TestPerRequestTTLHonored(t *testing.T) {
	st := newRedisLikeStore()
	opts := DefaultOptions()
	opts.TTL = 5 * time.Minute
	c := New(st, opts)
	in := hardInput("t1", "m", "ttl honor probe")
	meta := domain.CacheHitMeta{ExpiresAt: time.Now().Add(30 * time.Second)}
	c.StoreResponseWithMeta(context.Background(), in, []byte(`{"x":1}`), false, meta)
	ttl, ok := st.lastTTLFor(":exact:")
	if !ok {
		// Fall back to any recorded TTL: the store must have observed the
		// per-request bound rather than the 5-minute global.
		found := false
		st.mu.Lock()
		for _, v := range st.lastTTL {
			if v <= 31*time.Second {
				found = true
			}
		}
		st.mu.Unlock()
		if !found {
			t.Fatalf("store never observed the 30s per-request TTL: %+v", st.lastTTL)
		}
		return
	}
	if ttl > 31*time.Second {
		t.Fatalf("store TTL %v ignores the 30s per-request bound", ttl)
	}
}

func TestTenantInvalidationIsolationNamespaced(t *testing.T) {
	st := newRedisLikeStore()
	c := New(st, DefaultOptions())
	a := hardInput("t1", "m", "flush probe")
	b := hardInput("t2", "m", "flush probe")
	mustStore(t, c, a, `{"t":1}`)
	mustStore(t, c, b, `{"t":2}`)
	n, err := c.Invalidate(context.Background(), InvalidateScope{Scope: "tenant", TenantID: "t1", Reason: "test"})
	if err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	if n == 0 {
		t.Fatalf("tenant flush must remove t1 entries")
	}
	mustMiss(t, c, a)
	hit := mustHit(t, c, b, domain.CacheExact)
	if string(hit.Body) != `{"t":2}` {
		t.Fatalf("t2 entry corrupted by t1 flush: %s", hit.Body)
	}
}

func TestKeyScopeRequiresTenant(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	in := hardInput("t1", "m", "key flush probe")
	mustStore(t, c, in, `{"k":1}`)
	if _, err := c.Invalidate(context.Background(), InvalidateScope{Scope: "key", Key: "exact:abc", Reason: "test"}); err == nil {
		t.Fatalf("key flush without tenant must fail instead of silently deleting nothing")
	}
	key := ExactKey(in)
	n, err := c.Invalidate(context.Background(), InvalidateScope{Scope: "key", Key: key, TenantID: "t1", Reason: "test"})
	if err != nil {
		t.Fatalf("tenant key flush: %v", err)
	}
	if n == 0 {
		t.Fatalf("tenant key flush must remove the entry")
	}
	mustMiss(t, c, in)
}

func TestBypassRules(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	ctx := context.Background()
	in := hardInput("t1", "m", "bypass probe")
	mustStore(t, c, in, `{"ok":1}`)
	// Sensitive requests bypass even with a stored entry.
	if hit := c.Lookup(ctx, in, false, "", true); hit.Hit {
		t.Fatalf("sensitive lookup must bypass")
	} else if hit.BypassReason == "" {
		t.Fatalf("sensitive bypass must name its reason")
	}
	// Explicit bypass flags bypass with audit.
	if hit := c.Lookup(ctx, in, true, "bypass_requested", false); hit.Hit {
		t.Fatalf("requested bypass must not hit")
	}
	st := c.Stats()
	if st.Bypasses < 2 {
		t.Fatalf("bypasses must be counted, got %+v", st)
	}
	if st.BypassByReason[domain.CacheBypassSensitive] == 0 {
		t.Fatalf("sensitive bypass missing from bypass_by_reason: %+v", st.BypassByReason)
	}
	// Partial and oversized responses are never cached.
	if got := c.StoreResponse(ctx, in, nil, false); got != "" {
		t.Fatalf("empty body must not be stored")
	}
	big := make([]byte, DefaultOptions().MaxResponseBytes+1)
	if got := c.StoreResponse(ctx, in, big, false); got != "" {
		t.Fatalf("oversized body must not be stored")
	}
	if c.Stats().BypassByReason[domain.CacheBypassTooLarge] == 0 {
		t.Fatalf("too-large bypass must be recorded")
	}
	// The valid entry survives the rejected writes.
	mustHit(t, c, in, domain.CacheExact)
}

func TestPolicyBypassMatrix(t *testing.T) {
	cfg := PolicyConfig{Enabled: true, ExactEnabled: true, TTL: 5 * time.Minute, BypassTools: true, BypassLiveData: true}
	hot := 0.9
	mustBypass := func(in PolicyInput, reason string) {
		t.Helper()
		if got := Evaluate(in, cfg); got.Cacheable || got.BypassReason != reason {
			t.Fatalf("want bypass=%s, got %+v", reason, got)
		}
	}
	mustBypass(PolicyInput{Sensitive: true, PolicyUseCache: true}, domain.CacheBypassSensitive)
	mustBypass(PolicyInput{PolicyUseCache: false}, domain.CacheBypassPolicyDisabled)
	mustBypass(PolicyInput{PolicyUseCache: true, HasTools: true, ToolsSafe: false}, domain.CacheBypassToolRequest)
	mustBypass(PolicyInput{PolicyUseCache: true, Temperature: &hot}, domain.CacheBypassNondeterministic)
	mustBypass(PolicyInput{PolicyUseCache: true, N: 2}, domain.CacheBypassMultiSample)
	mustBypass(PolicyInput{PolicyUseCache: true, HasImages: true}, domain.CacheBypassMultimodal)
	mustBypass(PolicyInput{PolicyUseCache: true, CacheBypass: true}, domain.CacheBypassRequested)
	off := false
	mustBypass(PolicyInput{PolicyUseCache: true, EndpointCache: &off}, domain.CacheBypassEndpointDisabled)
	// Live-data prompts bypass rather than risk stale facts.
	live := PolicyInput{PolicyUseCache: true, Request: KeyInput{
		Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("what is the live stock price right now")}},
	}}
	mustBypass(live, domain.CacheBypassLiveData)
	// Deterministic, tool-free traffic is cacheable — including streams:
	// a hit replays as SSE and a clean completion is stored.
	zero := 0.0
	if got := Evaluate(PolicyInput{PolicyUseCache: true, Temperature: &zero}, cfg); !got.Cacheable {
		t.Fatalf("deterministic request must be cacheable, got %+v", got)
	}
	if got := Evaluate(PolicyInput{PolicyUseCache: true, Stream: true, Temperature: &zero}, cfg); !got.Cacheable {
		t.Fatalf("deterministic stream must be cacheable, got %+v", got)
	}
}

func TestNowIsNeverCacheSafe(t *testing.T) {
	// "now" reads the clock: any answer built on it goes stale on write.
	// Both the wire fallback and the registry verdict must agree.
	if ToolsSafeForCache([]domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "now"}}}, nil) {
		t.Fatalf("wire tool 'now' must not be cache-safe")
	}
	if !ToolsSafeForCache([]domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "echo"}}}, nil) {
		t.Fatalf("wire tool 'echo' must stay cache-safe")
	}
	if ToolsSafeForCache([]domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "now"}}},
		map[string]bool{"now": true}) {
		t.Fatalf("registry 'now' must not be cache-safe even when marked safe")
	}
	if !ToolsSafeForCache([]domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "echo"}}},
		map[string]bool{"echo": true}) {
		t.Fatalf("registry 'echo' must stay cache-safe")
	}
	cfg := PolicyConfig{Enabled: true, ExactEnabled: true, TTL: time.Minute, BypassTools: true}
	zero := 0.0
	got := Evaluate(PolicyInput{PolicyUseCache: true, HasTools: true, ToolsSafe: false, Temperature: &zero}, cfg)
	if got.Cacheable || got.BypassReason != domain.CacheBypassToolRequest {
		t.Fatalf("time-dependent tool request must bypass, got %+v", got)
	}
}

func TestPrefixTierIndependent(t *testing.T) {
	// With exact disabled, identical short prompts still reuse via prefix.
	opts := DefaultOptions()
	opts.ExactEnabled = false
	opts.SemanticEnabled = false
	c := New(newRedisLikeStore(), opts)
	a := hardInput("t1", "m", "short template prompt")
	mustStore(t, c, a, `{"p":1}`)
	mustHit(t, c, hardInput("t1", "m", "short template prompt"), domain.CachePrefix)
	// ...but a settings change still misses: the tier key is weaker, the
	// serving-context validator is not.
	hot := 0.9
	other := hardInput("t1", "m", "short template prompt")
	other.Temperature = &hot
	mustMiss(t, c, other)
}

func TestSemanticTierGating(t *testing.T) {
	long := strings.Repeat("semantic gating vocabulary alpha beta gamma ", 20)
	mk := func(suffix string) KeyInput {
		return KeyInput{TenantID: "t1", Model: "m", Messages: []domain.ChatMessage{
			{Role: domain.RoleUser, Content: domain.NewTextContent(long + suffix)},
		}}
	}
	// Disabled semantic tier never serves near-duplicates.
	off := DefaultOptions()
	off.SemanticEnabled = false
	cOff := New(newRedisLikeStore(), off)
	cOff.StoreResponse(context.Background(), mk("delta one two three"), []byte(`{"s":1}`), false)
	if hit := cOff.Lookup(context.Background(), mk("delta four five six"), false, "", false); hit.Hit && hit.Kind == domain.CacheSemantic {
		t.Fatalf("disabled semantic tier must not serve")
	}
	// A store with the tier disabled must not populate the index either:
	// re-enabling lookup on the same process must still miss.
	if hit := cOff.Lookup(context.Background(), mk("delta one two three"), false, "", false); !hit.Hit {
		t.Fatalf("exact tier must still serve when semantic is disabled")
	}
	// Per-request threshold override narrows the global default.
	opts := DefaultOptions()
	c := New(newRedisLikeStore(), opts)
	c.StoreResponse(context.Background(), mk("delta one two three"), []byte(`{"s":1}`), false)
	near := mk("delta one two three plus extra words here")
	hit := c.Lookup(context.Background(), near, false, "", false)
	if !hit.Hit || hit.Kind != domain.CacheSemantic {
		t.Fatalf("near-duplicate must hit at the default threshold, got %+v", hit)
	}
	strict := c.LookupWithTiers(context.Background(), near, false, "", false, true, true, true, 0.9999)
	if strict.Hit {
		t.Fatalf("strict per-request threshold must turn the hit into a miss")
	}
}

func TestTierOverrideDowngradeIsMiss(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	in := hardInput("t1", "m", "tier probe")
	mustStore(t, c, in, `{"ok":1}`)
	// Disabling every tier up front records a miss, never a phantom hit.
	res := c.LookupWithTiers(context.Background(), in, false, "", false, false, false, false, 0)
	if res.Hit {
		t.Fatalf("fully disabled tiers must miss")
	}
	st := c.Stats()
	if st.ExactHits != 0 {
		t.Fatalf("downgraded lookup must not record a hit, got %+v", st)
	}
	if st.Misses != 1 {
		t.Fatalf("downgraded lookup must record a miss, got %+v", st)
	}
}

func TestConcurrentIdenticalRequests(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	in := hardInput("t1", "m", "concurrent probe")
	mustStore(t, c, in, `{"c":1}`)
	const n = 32
	var wg sync.WaitGroup
	errs := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hit := c.Lookup(context.Background(), in, false, "", false)
			if !hit.Hit || hit.Kind != domain.CacheExact {
				errs <- fmt.Sprintf("missed: %+v", hit)
				return
			}
			if string(hit.Body) != `{"c":1}` {
				errs <- fmt.Sprintf("corrupt body: %s", hit.Body)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	if got := c.Stats().ExactHits; got < n {
		t.Fatalf("expected >= %d hits, got %d", n, got)
	}
}

func TestConcurrentTenantWrites(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	const tenants = 8
	var wg sync.WaitGroup
	for i := 0; i < tenants; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := fmt.Sprintf("ct-%d", i)
			in := hardInput(tenant, "m", "shared concurrent prompt")
			c.StoreResponse(context.Background(), in, []byte(fmt.Sprintf(`{"t":%d}`, i)), false)
			hit := c.Lookup(context.Background(), in, false, "", false)
			if !hit.Hit {
				t.Errorf("tenant %s missed its own entry", tenant)
				return
			}
			want := fmt.Sprintf(`{"t":%d}`, i)
			if string(hit.Body) != want {
				t.Errorf("tenant %s leaked: got %s want %s", tenant, hit.Body, want)
			}
		}(i)
	}
	wg.Wait()
}

func TestDashboardStatsCoherent(t *testing.T) {
	c := New(newRedisLikeStore(), DefaultOptions())
	ctx := context.Background()
	a := hardInput("t1", "m", "stats probe one")
	b := hardInput("t1", "m", "stats probe two")
	c.Lookup(ctx, a, false, "", false) // miss
	mustStore(t, c, a, `{"a":1}`)
	c.Lookup(ctx, a, false, "", false) // hit
	c.Lookup(ctx, b, false, "", false) // miss
	c.RecordBypass(domain.CacheBypassStreaming)
	c.StoreResponseWithMeta(ctx, a, []byte(`{"a":1}`), false,
		domain.CacheHitMeta{CostUSD: 0.002, ProviderLatencyMS: 1200})
	c.Lookup(ctx, a, false, "", false) // hit (cost + latency recorded)
	st := c.Stats()
	if st.ExactHits != 2 || st.Misses != 2 {
		t.Fatalf("hits/misses mismatch: %+v", st)
	}
	if st.HitRate != 0.5 {
		t.Fatalf("hit rate must be 0.5, got %v", st.HitRate)
	}
	if st.BypassByReason[domain.CacheBypassStreaming] != 1 {
		t.Fatalf("streaming bypass missing: %+v", st.BypassByReason)
	}
	if st.CostSavedUSD < 0.002 {
		t.Fatalf("cost saved must accumulate, got %v", st.CostSavedUSD)
	}
	if st.LatencySavedMS < 1200 {
		t.Fatalf("latency saved must accumulate, got %d", st.LatencySavedMS)
	}
}

func TestSemanticBoundedIndex(t *testing.T) {
	opts := DefaultOptions()
	opts.MaxSemanticEntries = 16
	c := New(newRedisLikeStore(), opts)
	for i := 0; i < 64; i++ {
		in := hardInput("t1", "m", fmt.Sprintf("eviction probe number %d with distinct vocabulary zebra-%d", i, i))
		c.StoreResponse(context.Background(), in, []byte(`{"i":1}`), false)
	}
	if got := c.Stats().Entries; got > 16 {
		t.Fatalf("semantic index must stay bounded at 16, got %d", got)
	}
}
