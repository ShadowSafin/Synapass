package cache

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Store is the fast body backend (Redis in production, memory in tests).
type Store interface {
	Get(ctx context.Context, key string) ([]byte, bool, error)
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	DeletePrefix(ctx context.Context, prefix string) (int, error)
}

// Options configures caching.
type Options struct {
	Enabled          bool
	TTL              time.Duration
	MaxResponseBytes int
	// SemanticThreshold is the cosine similarity in [0,1] for a semantic hit.
	SemanticThreshold float64
	// PrefixLength bounds prefix-tier response reuse (short prompts only).
	PrefixLength int
	// ExactEnabled gates exact reuse independently of the master switch.
	ExactEnabled bool
	// SemanticEnabled gates the semantic tier.
	SemanticEnabled bool
	// PrefixEnabled gates the prefix tier.
	PrefixEnabled bool
	// MaxSemanticEntries bounds the in-process semantic index.
	MaxSemanticEntries int
	// BypassTools skips tool-carrying requests (default true).
	BypassTools bool
	// AllowNondeterministic permits caching temp>0 unseeded requests.
	AllowNondeterministic bool
	// BypassLiveData skips live-data-looking prompts (default true).
	BypassLiveData bool
}

// DefaultOptions returns production-minded defaults.
func DefaultOptions() Options {
	return Options{
		Enabled:           true,
		ExactEnabled:      true,
		SemanticEnabled:   true,
		PrefixEnabled:     true,
		TTL:               5 * time.Minute,
		MaxResponseBytes:  256 << 10,
		SemanticThreshold: 0.92,
		PrefixLength:      256,
		MaxSemanticEntries: 2000,
		BypassTools:       true,
		BypassLiveData:    true,
	}
}

// Cache is the lookup/store facade.
type Cache struct {
	store Store
	opts  Options

	mu sync.Mutex
	// memIndex tracks semantic entries for similarity search when the store
	// cannot enumerate keys (Redis SCAN is expensive on the hot path, so the
	// gateway keeps a small in-process embedding index).
	memIndex map[string]semanticEntry
	// keyIndex maps a request's primary cache key to every store key
	// written for it (exact + prefix + semantic tiers), so a key-scope
	// flush evicts the whole logical entry rather than just the tier the
	// operator happened to name.
	keyIndex map[string]keyRef
	stats    domain.CacheStats
	lookups  int64
	lookupMS float64
}

type keyRef struct {
	storeKeys []string
	storedAt  time.Time
}

type semanticEntry struct {
	key       string
	nsKey     string
	embedding map[string]float64
	body      []byte
	expiresAt time.Time
	storedAt  time.Time
	tenant    string
	input     KeyInput
}

// New creates a cache.
func New(store Store, opts Options) *Cache {
	if opts.TTL <= 0 {
		opts.TTL = 5 * time.Minute
	}
	if opts.SemanticThreshold <= 0 {
		opts.SemanticThreshold = 0.92
	}
	if opts.PrefixLength <= 0 {
		opts.PrefixLength = 256
	}
	if opts.MaxSemanticEntries <= 0 {
		opts.MaxSemanticEntries = 2000
	}
	return &Cache{store: store, opts: opts, memIndex: map[string]semanticEntry{}, keyIndex: map[string]keyRef{}}
}

// Enabled reports whether caching is active.
func (c *Cache) Enabled() bool { return c != nil && c.opts.Enabled }

// Options returns a copy of the options.
func (c *Cache) Options() Options {
	if c == nil {
		return DefaultOptions()
	}
	return c.opts
}

// Stats returns a copy of hit/miss counters.
func (c *Cache) Stats() domain.CacheStats {
	if c == nil {
		return domain.CacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	total := s.ExactHits + s.ExactMisses + s.PrefixHits + s.SemanticHits
	if s.Misses > 0 {
		total = s.ExactHits + s.PrefixHits + s.SemanticHits + s.Misses
		if s.ExactMisses == 0 {
			s.ExactMisses = s.Misses
		}
	}
	if total > 0 {
		s.HitRate = float64(s.ExactHits+s.PrefixHits+s.SemanticHits) / float64(total)
	}
	s.Entries = int64(len(c.keyIndex))
	if len(c.memIndex) > len(c.keyIndex) {
		// Semantic-only deployments: every index entry is still a live
		// serving candidate.
		s.Entries = int64(len(c.memIndex))
	}
	if c.lookups > 0 {
		s.LookupMSAvg = c.lookupMS / float64(c.lookups)
	}
	if s.BypassByReason == nil {
		s.BypassByReason = map[string]int64{}
	}
	return s
}

// namespaced scopes a bare kind:key under the tenant namespace. All Phase 5
// bodies live under tenant:<id>:<kind>:<hash>; legacy bare keys are read for
// compatibility but never written.
func namespaced(tenantID, bare string) string {
	if tenantID == "" {
		return bare
	}
	return "tenant:" + tenantID + ":" + bare
}

// Lookup checks exact, then prefix (short prompts only), then semantic.
func (c *Cache) Lookup(ctx context.Context, in KeyInput, bypass bool, bypassReason string, sensitive bool) domain.CacheLookupResult {
	return c.LookupWithTiers(ctx, in, bypass, bypassReason, sensitive, true, true, true, 0)
}

// TierOverride carries per-request tier allowances (from a scoped cache
// policy) into the lookup. A non-positive SemThreshold keeps the global
// default.
type TierOverride struct {
	AllowExact   bool
	AllowPrefix  bool
	AllowSemantic bool
	SemThreshold float64
}

// LookupWithTiers is Lookup with per-request tier enforcement applied
// before any store read. Enforcing here (rather than downgrading a hit
// afterwards) keeps the hit/miss counters honest: a tier-disabled entry
// is a miss, never a phantom hit.
func (c *Cache) LookupWithTiers(ctx context.Context, in KeyInput, bypass bool, bypassReason string, sensitive bool, allowExact, allowPrefix, allowSemantic bool, semThreshold float64) domain.CacheLookupResult {
	start := time.Now()
	if c == nil || !c.opts.Enabled || bypass || sensitive {
		if c != nil {
			c.recordBypass(bypassReason, sensitive)
		}
		reason := bypassReason
		if sensitive && reason == "" {
			reason = domain.CacheBypassSensitive
		}
		if reason == "" {
			reason = domain.CacheBypassDisabled
		}
		return domain.CacheLookupResult{Hit: false, BypassReason: reason, Trace: &domain.CacheTrace{Hit: false, BypassReason: reason}}
	}
	// Exact tier.
	if c.opts.ExactEnabled && allowExact {
		exact := ExactKey(in)
		ns := namespaced(in.TenantID, exact)
		if body, ok, _ := c.get(ctx, ns); ok {
			if payload, meta, ok2 := decodePayload(body); ok2 {
				if validFor(meta, in) {
					c.recordHit(domain.CacheExact, meta, msSince(start))
					return hitResult(domain.CacheExact, ns, payload, meta, start, 0)
				}
			} else if len(body) > 0 {
				// Legacy raw body without an envelope.
				c.recordHit(domain.CacheExact, domain.CacheHitMeta{}, msSince(start))
				return hitResult(domain.CacheExact, ns, body, domain.CacheHitMeta{}, start, 0)
			}
		}
		// Legacy bare key for rolling upgrades.
		if in.TenantID != "" {
			if body, ok, _ := c.get(ctx, exact); ok {
				if payload, meta, ok2 := decodePayload(body); ok2 {
					if validFor(meta, in) {
						c.recordHit(domain.CacheExact, meta, msSince(start))
						return hitResult(domain.CacheExact, exact, payload, meta, start, 0)
					}
				} else if len(body) > 0 {
					c.recordHit(domain.CacheExact, domain.CacheHitMeta{}, msSince(start))
					return hitResult(domain.CacheExact, exact, body, domain.CacheHitMeta{}, start, 0)
				}
			}
		}
	}
	// Prefix tier: short prompts only. A shared 256-char head with a
	// different tail must never serve as a hit — that was the Phase 2
	// correctness gap this gate closes.
	if c.opts.PrefixEnabled && allowPrefix && in.NormalizedLength() <= c.opts.PrefixLength {
		prefix := PrefixKey(in, c.opts.PrefixLength)
		ns := namespaced(in.TenantID, prefix)
		if body, ok, _ := c.get(ctx, ns); ok {
			if payload, meta, ok2 := decodePayload(body); ok2 {
				if validFor(meta, in) {
					c.recordHit(domain.CachePrefix, meta, msSince(start))
					return hitResult(domain.CachePrefix, ns, payload, meta, start, 0)
				}
			} else if len(body) > 0 {
				c.recordHit(domain.CachePrefix, domain.CacheHitMeta{}, msSince(start))
				return hitResult(domain.CachePrefix, ns, body, domain.CacheHitMeta{}, start, 0)
			}
		}
	}
	// Semantic tier: cosine over word-bag embeddings, tenant-isolated.
	if c.opts.SemanticEnabled && allowSemantic {
		threshold := c.opts.SemanticThreshold
		if semThreshold > 0 {
			threshold = semThreshold
		}
		emb := embed(in)
		bestKey, bestBody, bestSim, bestMeta := c.semanticSearch(in, emb)
		if bestSim >= threshold && bestBody != nil {
			if payload, meta, ok := decodePayload(bestBody); ok {
				if validFor(meta, in) {
					c.recordHit(domain.CacheSemantic, meta, msSince(start))
					return hitResult(domain.CacheSemantic, bestKey, payload, meta, start, bestSim)
				}
			} else {
				c.recordHit(domain.CacheSemantic, bestMeta, msSince(start))
				return hitResult(domain.CacheSemantic, bestKey, bestBody, bestMeta, start, bestSim)
			}
		}
	}
	c.recordMiss(start)
	return domain.CacheLookupResult{Hit: false, Trace: &domain.CacheTrace{Hit: false, Key: ExactKey(in), LookupMS: msSince(start)}}
}

// StoreResponse writes exact + prefix (short only) + semantic entries as
// envelopes carrying serving metadata for future validation.
func (c *Cache) StoreResponse(ctx context.Context, in KeyInput, body []byte, sensitive bool) string {
	return c.StoreResponseWithMeta(ctx, in, body, sensitive, domain.CacheHitMeta{})
}

// StoreResponseWithMeta stores with explicit serving metadata.
func (c *Cache) StoreResponseWithMeta(ctx context.Context, in KeyInput, body []byte, sensitive bool, meta domain.CacheHitMeta) string {
	if c == nil || !c.opts.Enabled || sensitive {
		return ""
	}
	if len(body) == 0 {
		return ""
	}
	if c.opts.MaxResponseBytes > 0 && len(body) > c.opts.MaxResponseBytes {
		c.recordBypass(domain.CacheBypassTooLarge, false)
		return ""
	}
	now := time.Now()
	if meta.PromptHash == "" {
		meta.PromptHash = in.PromptHash()
	}
	if meta.StoredAt.IsZero() {
		meta.StoredAt = now
	}
	// The effective TTL honors a per-request override (from a scoped cache
	// policy): the Redis TTL and the in-process expiry both follow the
	// envelope's ExpiresAt when it is sooner than the global default, so
	// the dashboard TTL and the real eviction agree.
	ttl := c.opts.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	if meta.ExpiresAt.IsZero() {
		meta.ExpiresAt = now.Add(ttl)
	} else {
		remaining := time.Until(meta.ExpiresAt)
		if remaining <= 0 {
			return ""
		}
		if remaining < ttl {
			ttl = remaining
		}
	}
	if meta.Model == "" {
		meta.Model = in.Model
	}
	if meta.Provider == "" {
		meta.Provider = in.Provider
	}
	if meta.PolicyID == "" {
		meta.PolicyID = in.PolicyID
	}
	if meta.PolicyVersion == 0 {
		meta.PolicyVersion = in.PolicyVersion
	}
	if meta.EndpointID == "" {
		meta.EndpointID = in.EndpointID
	}
	if meta.ToolsHash == "" {
		meta.ToolsHash = in.ToolsHash()
	}
	if meta.SettingsHash == "" {
		meta.SettingsHash = in.SettingsHash()
	}
	if meta.Sensitivity == "" {
		meta.Sensitivity = in.Sensitivity
	}
	if meta.User == "" {
		meta.User = in.User
	}
	if meta.APIKeyID == "" {
		meta.APIKeyID = in.APIKeyID
	}
	envelope, err := json.Marshal(domain.CachedPayload{Body: body, Meta: meta})
	if err != nil {
		return ""
	}
	// Collect every store key written so the reverse index can evict the
	// whole logical entry on a key-scope flush. The returned primary is
	// the exact key when that tier is on, else the first tier actually
	// written; "" means nothing was written at all.
	var written []string
	primary := ""
	if c.opts.ExactEnabled {
		exact := ExactKey(in)
		ns := namespaced(in.TenantID, exact)
		_ = c.set(ctx, ns, envelope, ttl)
		written = append(written, ns)
		primary = exact
	}
	if c.opts.PrefixEnabled && in.NormalizedLength() <= c.opts.PrefixLength {
		prefix := PrefixKey(in, c.opts.PrefixLength)
		ns := namespaced(in.TenantID, prefix)
		_ = c.set(ctx, ns, envelope, ttl)
		written = append(written, ns)
		if primary == "" {
			primary = prefix
		}
	}
	// Semantic index. Gated on the tier flag so a disabled tier neither
	// spends memory nor leaks entries into future lookups.
	if c.opts.SemanticEnabled {
		emb := embed(in)
		embHash := embeddingHash(emb)
		semKey := "semantic:" + embHash
		nsSem := namespaced(in.TenantID, semKey)
		_ = c.set(ctx, nsSem, envelope, ttl)
		written = append(written, nsSem)
		if primary == "" {
			primary = semKey
		}
		c.mu.Lock()
		c.memIndex[nsSem] = semanticEntry{
			key: semKey, nsKey: nsSem, embedding: emb, body: envelope,
			expiresAt: now.Add(ttl), storedAt: now, tenant: in.TenantID, input: in,
		}
		c.mu.Unlock()
	}
	if primary != "" {
		c.mu.Lock()
		if c.keyIndex == nil {
			c.keyIndex = map[string]keyRef{}
		}
		indexKey := primary
		if !strings.HasPrefix(indexKey, "tenant:") && in.TenantID != "" {
			indexKey = namespaced(in.TenantID, primary)
		}
		c.keyIndex[indexKey] = keyRef{storeKeys: written, storedAt: now}
		// The bare form resolves too, so a key flush naming either form
		// finds the entry.
		if indexKey != primary {
			c.keyIndex[primary] = keyRef{storeKeys: written, storedAt: now}
		}
		c.evictSemanticLocked()
		c.mu.Unlock()
	}
	return primary
}

// InvalidateTenant removes tenant-scoped entries (legacy entry point kept
// for compatibility; prefer Invalidate with an explicit scope).
func (c *Cache) InvalidateTenant(ctx context.Context, tenantID string) (int, error) {
	if c == nil {
		return 0, nil
	}
	if tenantID == "" {
		return c.Invalidate(ctx, InvalidateScope{Scope: "all", Reason: domain.CacheInvalidateManual})
	}
	return c.Invalidate(ctx, InvalidateScope{Scope: "tenant", TenantID: tenantID, Reason: domain.CacheInvalidateManual})
}

func (c *Cache) get(ctx context.Context, key string) ([]byte, bool, error) {
	if c == nil || c.store == nil {
		return nil, false, nil
	}
	return c.store.Get(ctx, key)
}

func (c *Cache) set(ctx context.Context, key string, body []byte, ttl time.Duration) error {
	if c == nil || c.store == nil {
		return nil
	}
	return c.store.Set(ctx, key, body, ttl)
}

func (c *Cache) semanticSearch(in KeyInput, emb map[string]float64) (string, []byte, float64, domain.CacheHitMeta) {
	c.mu.Lock()
	defer c.mu.Unlock()
	best := 0.0
	var bestKey string
	var bestBody []byte
	var bestMeta domain.CacheHitMeta
	now := time.Now()
	for k, e := range c.memIndex {
		if now.After(e.expiresAt) {
			delete(c.memIndex, k)
			continue
		}
		if in.TenantID != "" && e.tenant != "" && e.tenant != in.TenantID {
			continue
		}
		if in.Model != "" && e.input.Model != "" && !equalFold(e.input.Model, in.Model) {
			continue
		}
		// Pre-filter candidates through the same serving-context
		// validator the hit path uses (provider, policy, tools,
		// settings, sensitivity, user, key, endpoint, expiry). A
		// near-duplicate prompt under different serving rules must
		// never outrank a compatible entry: without this, the best
		// cosine match could shadow a valid hit and turn into a
		// wrong answer instead of a miss.
		if payload, meta, ok := decodePayload(e.body); ok {
			_ = payload
			if !validFor(meta, in) {
				continue
			}
			bestMeta = meta
		} else if len(e.body) == 0 {
			continue
		}
		sim := cosine(emb, e.embedding)
		if sim > best {
			best = sim
			bestKey = e.nsKey
			bestBody = e.body
			if payload, meta, ok := decodePayload(e.body); ok {
				_ = payload
				bestMeta = meta
			}
		}
	}
	return bestKey, bestBody, best, bestMeta
}

// evictSemanticLocked bounds the in-process semantic index. Expired entries
// go first; when the index is still over budget (high-traffic tenants with
// long TTLs), the oldest entries are evicted down to 90% of the cap so one
// insert does not trigger an eviction on every subsequent insert.
// The caller must hold c.mu.
func (c *Cache) evictSemanticLocked() {
	max := c.opts.MaxSemanticEntries
	if max <= 0 {
		return
	}
	now := time.Now()
	for k, e := range c.memIndex {
		if now.After(e.expiresAt) {
			delete(c.memIndex, k)
		}
	}
	if len(c.memIndex) <= max && len(c.keyIndex) <= max {
		return
	}
	// Trim to 90% so one insert does not trigger a full scan on every
	// subsequent insert while the index sits at the cap.
	target := max * 9 / 10
	for len(c.memIndex) > target {
		oldest := ""
		var oldestAt time.Time
		first := true
		for k, e := range c.memIndex {
			if first || e.storedAt.Before(oldestAt) {
				oldest, oldestAt, first = k, e.storedAt, false
			}
		}
		if oldest == "" {
			break
		}
		delete(c.memIndex, oldest)
	}
	// The reverse key index is bounded by the same budget: without this,
	// exact-disabled deployments would grow it without bound.
	for len(c.keyIndex) > target {
		oldest := ""
		var oldestAt time.Time
		first := true
		for k, ref := range c.keyIndex {
			if first || ref.storedAt.Before(oldestAt) {
				oldest, oldestAt, first = k, ref.storedAt, false
			}
		}
		if oldest == "" {
			break
		}
		delete(c.keyIndex, oldest)
	}
}

func (c *Cache) recordHit(kind domain.CacheKind, meta domain.CacheHitMeta, lookupMS float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch kind {
	case domain.CacheExact:
		c.stats.ExactHits++
	case domain.CachePrefix:
		c.stats.PrefixHits++
	case domain.CacheSemantic:
		c.stats.SemanticHits++
	}
	c.stats.ReuseCount++
	c.lookups++
	c.lookupMS += lookupMS
	if meta.ProviderLatencyMS > 0 {
		c.stats.LatencySavedMS += meta.ProviderLatencyMS
	}
	if meta.CostUSD > 0 {
		c.stats.CostSavedUSD += meta.CostUSD
	}
}

func (c *Cache) recordMiss(start time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.ExactMisses++
	c.stats.Misses++
	c.lookups++
	c.lookupMS += msSince(start)
}

func (c *Cache) recordBypass(reason string, sensitive bool) {
	r := reason
	if sensitive && r == "" {
		r = domain.CacheBypassSensitive
	}
	if r == "" {
		r = domain.CacheBypassDisabled
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stats.Bypasses++
	if c.stats.BypassByReason == nil {
		c.stats.BypassByReason = map[string]int64{}
	}
	c.stats.BypassByReason[r]++
	c.lookups++
}

// RecordBypass records a bypass decided outside Lookup (streaming,
// policy-disabled, cache-disabled). The request flow bypasses before any
// store lookup, so without this the dashboard bypass counters would miss
// exactly the traffic operators ask about first.
func (c *Cache) RecordBypass(reason string) {
	if c == nil {
		return
	}
	c.recordBypass(reason, false)
}

func hitResult(kind domain.CacheKind, key string, body []byte, meta domain.CacheHitMeta, start time.Time, sim float64) domain.CacheLookupResult {
	elapsed := msSince(start)
	saved := meta.ProviderLatencyMS
	return domain.CacheLookupResult{
		Hit: true, Kind: kind, Body: body, Key: key, Similarity: sim,
		LookupMS: elapsed, ReuseCount: meta.ReuseCount + 1, LatencySavedMS: saved,
		Meta: &meta,
		Trace: &domain.CacheTrace{
			Hit: true, Kind: string(kind), Key: key, LookupMS: elapsed,
			Similarity: sim, ReuseCount: meta.ReuseCount + 1, LatencySavedMS: saved,
		},
	}
}

// decodePayload unwraps the Phase 5 envelope, falling back to raw bodies
// written before the envelope existed.
func decodePayload(raw []byte) ([]byte, domain.CacheHitMeta, bool) {
	var p domain.CachedPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, domain.CacheHitMeta{}, false
	}
	if len(p.Body) == 0 {
		return nil, domain.CacheHitMeta{}, false
	}
	return p.Body, p.Meta, true
}

// validFor checks the stored serving context against the current request.
// A provider/model/policy/tool/settings/contract change turns a would-be
// hit into a miss rather than serving a stale answer.
//
// Every dimension that participates in ExactKey is re-checked here because
// the prefix and semantic tiers address entries by weaker keys: without
// these checks a shared prompt head or a near-duplicate prompt would serve
// an answer produced under different generation settings, a different
// response contract, another provider, or another user. Empty stored
// fields are treated as unknown (legacy envelopes predate them) and pass,
// except where the lookup itself carries the dimension — in that case an
// empty stored field can only come from a legacy entry and the safe
// direction is still to serve only when nothing contradicts.
func validFor(meta domain.CacheHitMeta, in KeyInput) bool {
	if meta.Model != "" && !equalFold(meta.Model, in.Model) {
		return false
	}
	// Provider: only comparable when both sides are known. Lookup keys are
	// built pre-route (no provider hint), so this fires for callers that
	// pass one and for prefix/semantic candidates stored under a provider.
	if meta.Provider != "" && in.Provider != "" && !equalFold(meta.Provider, in.Provider) {
		return false
	}
	// The remaining dimensions are derived deterministically from the
	// request on both the store and lookup paths, so strict equality
	// applies: any difference — including a dimension present on one
	// side only — turns the hit into a miss. That is the safe direction
	// (a miss costs one provider call; a false hit serves a wrong
	// answer), and it is what makes the weaker prefix/semantic tiers as
	// strict as the exact tier about serving context.
	if meta.PolicyID != in.PolicyID {
		return false
	}
	if meta.PolicyVersion != in.PolicyVersion {
		return false
	}
	if meta.ToolsHash != in.ToolsHash() {
		return false
	}
	// Generation settings + response contract: temperature, top-p, seed,
	// max tokens, response format, stop sequences. A mismatch changes the
	// distribution, so reuse is incorrect.
	if meta.SettingsHash != in.SettingsHash() {
		return false
	}
	if meta.EndpointID != in.EndpointID {
		return false
	}
	// Sensitivity labels and end-user identity isolate exactly like
	// tenants: a stored label never serves a differently-labelled request.
	if meta.Sensitivity != in.Sensitivity {
		return false
	}
	if meta.User != in.User {
		return false
	}
	// API key: exact keys hash it, so this only matters for the weaker
	// tiers, where cross-key reuse would otherwise leak one key's traffic
	// into another's.
	if meta.APIKeyID != in.APIKeyID {
		return false
	}
	if !meta.ExpiresAt.IsZero() && time.Now().After(meta.ExpiresAt) {
		return false
	}
	return true
}

func equalFold(a, b string) bool {
	if a == b {
		return true
	}
	if len(a) != len(b) {
		// Compare case-insensitively without importing strings in hot path
		// callers; lengths equal is required for fold equality here.
		return foldEq(a, b)
	}
	return foldEq(a, b)
}

func foldEq(a, b string) bool {
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000.0
}
