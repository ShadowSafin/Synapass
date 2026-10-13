package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

// fakeStore is an in-memory KeyStore that records how often it is consulted, so
// tests can assert that the credential cache actually short-circuits lookups.
type fakeStore struct {
	mu sync.Mutex

	keys    map[string]*domain.APIKey // keyed by hash
	tenants map[string]*domain.Tenant // keyed by id
	touched map[string]int            // key id -> touch count
	err     error                     // when set, every method fails
	lookups int                       // LookupKey invocations
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		keys:    map[string]*domain.APIKey{},
		tenants: map[string]*domain.Tenant{},
		touched: map[string]int{},
	}
}

func (f *fakeStore) put(tenant *domain.Tenant, key *domain.APIKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys[key.KeyHash] = key
	f.tenants[tenant.ID] = tenant
}

func (f *fakeStore) LookupKey(_ context.Context, keyHash string) (*domain.APIKey, *domain.Tenant, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lookups++
	if f.err != nil {
		return nil, nil, f.err
	}
	key, ok := f.keys[keyHash]
	if !ok {
		return nil, nil, nil
	}
	tenant := f.tenants[key.TenantID]
	// Return copies so a test cannot accidentally mutate stored state through the
	// principal, which mirrors what a real store read does.
	keyCopy := *key
	var tenantCopy *domain.Tenant
	if tenant != nil {
		t := *tenant
		tenantCopy = &t
	}
	return &keyCopy, tenantCopy, nil
}

func (f *fakeStore) LookupByPrefix(_ context.Context, prefix string) ([]domain.APIKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.APIKey
	for _, key := range f.keys {
		if strings.HasPrefix(key.Prefix, prefix) {
			out = append(out, *key)
		}
	}
	return out, nil
}

func (f *fakeStore) TouchKey(_ context.Context, keyID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.touched[keyID]++
	return nil
}

func (f *fakeStore) lookupCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookups
}

func (f *fakeStore) touchCount(keyID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.touched[keyID]
}

// fakeCache is an in-memory Cache with real TTL semantics.
type fakeCache struct {
	mu    sync.Mutex
	items map[string]cacheItem
}

type cacheItem struct {
	value   []byte
	expires time.Time
}

func newFakeCache() *fakeCache { return &fakeCache{items: map[string]cacheItem{}} }

func (c *fakeCache) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	item, ok := c.items[key]
	if !ok {
		return nil, false, nil
	}
	if time.Now().After(item.expires) {
		delete(c.items, key)
		return nil, false, nil
	}
	return item.value, true, nil
}

func (c *fakeCache) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[key] = cacheItem{value: value, expires: time.Now().Add(ttl)}
	return nil
}

func (c *fakeCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
	return nil
}

// activeTenant builds a usable tenant.
func activeTenant(id string) *domain.Tenant {
	now := domain.Now()
	return &domain.Tenant{
		ID:        id,
		Slug:      id,
		Name:      id,
		Status:    domain.StatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// activeKey builds a usable key belonging to tenantID.
func activeKey(id, tenantID, plaintext string, scopes ...string) *domain.APIKey {
	return &domain.APIKey{
		ID:        id,
		TenantID:  tenantID,
		Name:      id,
		Prefix:    domain.DisplayPrefix(plaintext),
		KeyHash:   HashKey(plaintext),
		Scopes:    scopes,
		Status:    domain.APIKeyActive,
		CreatedAt: domain.Now(),
	}
}

// newAuthenticator pairs a store and cache with an authenticator under test.
func newAuthenticator(t *testing.T, opts Options) (*Authenticator, *fakeStore, *fakeCache) {
	t.Helper()
	store := newFakeStore()
	cache := newFakeCache()
	opts.Store = store
	if opts.Cache == nil {
		opts.Cache = cache
	}
	return New(opts), store, cache
}

// ---------------------------------------------------------------------------
// Token extraction
// ---------------------------------------------------------------------------

func TestExtractToken(t *testing.T) {
	cases := map[string]string{
		"Bearer syn_live_abc123":       "syn_live_abc123",
		"bearer syn_live_abc123":       "syn_live_abc123",
		"BEARER syn_live_abc123":       "syn_live_abc123",
		"  Bearer   syn_live_abc123  ": "syn_live_abc123",
		// A bare token is accepted because several SDK configurations and shell
		// one-liners send the key without the scheme.
		"syn_live_abc123": "syn_live_abc123",
		"":                "",
		"   ":             "",
	}

	for header, want := range cases {
		if got := ExtractToken(header); got != want {
			t.Errorf("ExtractToken(%q) = %q, want %q", header, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Authentication
// ---------------------------------------------------------------------------

func TestAuthenticateRejectsMissingCredentials(t *testing.T) {
	authenticator, _, _ := newAuthenticator(t, Options{})

	if _, err := authenticator.Authenticate(context.Background(), ""); !errors.Is(err, ErrMissingCredentials) {
		t.Fatalf("expected ErrMissingCredentials, got %v", err)
	}
}

func TestAuthenticateRejectsUndersizedTokenBeforeLookup(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{})

	_, err := authenticator.Authenticate(context.Background(), "short")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	if store.lookupCount() != 0 {
		t.Error("an undersized token must be rejected without touching the store")
	}
}

func TestAuthenticateRejectsUnknownKey(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{})

	token := "syn_live_" + strings.Repeat("z", 40)
	if _, err := authenticator.Authenticate(context.Background(), token); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
	if store.lookupCount() != 1 {
		t.Errorf("lookups = %d, want 1", store.lookupCount())
	}
}

func TestAuthenticateAcceptsValidKey(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{})
	token := "syn_live_" + strings.Repeat("a", 40)
	store.put(activeTenant("t-1"), activeKey("k-1", "t-1", token, string(domain.ScopeInference)))

	principal, err := authenticator.Authenticate(context.Background(), token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.TenantID() != "t-1" {
		t.Errorf("tenant = %q, want t-1", principal.TenantID())
	}
	if principal.KeyID() != "k-1" {
		t.Errorf("key = %q, want k-1", principal.KeyID())
	}
	if !principal.Has(domain.ScopeInference) {
		t.Error("the key should grant the inference scope")
	}
	if principal.Has(domain.ScopeAdminKeys) {
		t.Error("the key must not grant a scope it was not issued")
	}
	if principal.Label() != "k-1 ("+domain.DisplayPrefix(token)+")" {
		t.Errorf("label = %q", principal.Label())
	}
	if store.touchCount("k-1") != 1 {
		t.Errorf("last-used telemetry = %d, want 1", store.touchCount("k-1"))
	}
}

func TestAuthenticateAcceptsBearerScheme(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{})
	token := "syn_live_" + strings.Repeat("b", 40)
	store.put(activeTenant("t-1"), activeKey("k-1", "t-1", token, string(domain.ScopeInference)))

	principal, err := authenticator.Authenticate(context.Background(), ExtractToken("Bearer "+token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.KeyID() != "k-1" {
		t.Errorf("key = %q", principal.KeyID())
	}
}

func TestAuthenticateCachesSuccessfulLookup(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{CacheTTL: time.Minute})
	token := "syn_live_" + strings.Repeat("c", 40)
	store.put(activeTenant("t-1"), activeKey("k-1", "t-1", token, string(domain.ScopeInference)))

	for i := 0; i < 5; i++ {
		if _, err := authenticator.Authenticate(context.Background(), token); err != nil {
			t.Fatalf("Authenticate #%d: %v", i+1, err)
		}
	}

	if store.lookupCount() != 1 {
		t.Errorf("store lookups = %d, want 1 with a warm cache", store.lookupCount())
	}
}

func TestAuthenticateNegativeCacheShortCircuits(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{CacheTTL: time.Minute})
	token := "syn_live_" + strings.Repeat("d", 40)

	for i := 0; i < 4; i++ {
		if _, err := authenticator.Authenticate(context.Background(), token); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("expected ErrInvalidCredentials, got %v", err)
		}
	}

	// One lookup for the four attempts: a flood of one bad credential must not
	// become a flood of database reads.
	if store.lookupCount() != 1 {
		t.Errorf("store lookups = %d, want 1 with a negative cache entry", store.lookupCount())
	}
}

func TestAuthenticateAWorksWithoutCache(t *testing.T) {
	store := newFakeStore()
	authenticator := New(Options{Store: store})
	token := "syn_live_" + strings.Repeat("e", 40)
	store.put(activeTenant("t-1"), activeKey("k-1", "t-1", token, string(domain.ScopeInference)))

	for i := 0; i < 3; i++ {
		if _, err := authenticator.Authenticate(context.Background(), token); err != nil {
			t.Fatalf("Authenticate #%d: %v", i+1, err)
		}
	}
	if store.lookupCount() != 3 {
		t.Errorf("store lookups = %d, want one per request when uncached", store.lookupCount())
	}
}

func TestAuthenticateRejectsRevokedKey(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{})
	token := "syn_live_" + strings.Repeat("f", 40)

	key := activeKey("k-1", "t-1", token, string(domain.ScopeInference))
	key.Status = domain.APIKeyRevoked
	store.put(activeTenant("t-1"), key)

	if _, err := authenticator.Authenticate(context.Background(), token); !errors.Is(err, ErrKeyRevoked) {
		t.Fatalf("expected ErrKeyRevoked, got %v", err)
	}
}

func TestAuthenticateRejectsExpiredKey(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{})
	token := "syn_live_" + strings.Repeat("g", 40)

	past := domain.Now().Add(-time.Hour)
	key := activeKey("k-1", "t-1", token, string(domain.ScopeInference))
	key.ExpiresAt = &past
	store.put(activeTenant("t-1"), key)

	if _, err := authenticator.Authenticate(context.Background(), token); !errors.Is(err, ErrKeyExpired) {
		t.Fatalf("expected ErrKeyExpired, got %v", err)
	}
}

func TestAuthenticateRejectsInactiveTenant(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{})
	token := "syn_live_" + strings.Repeat("h", 40)

	tenant := activeTenant("t-1")
	tenant.Status = domain.StatusDisabled
	store.put(tenant, activeKey("k-1", "t-1", token, string(domain.ScopeInference)))

	if _, err := authenticator.Authenticate(context.Background(), token); !errors.Is(err, ErrTenantInactive) {
		t.Fatalf("expected ErrTenantInactive, got %v", err)
	}
}

func TestAuthenticateSurfacesStoreFailure(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{})
	store.err = errors.New("connection refused")

	token := "syn_live_" + strings.Repeat("i", 40)
	_, err := authenticator.Authenticate(context.Background(), token)

	normalized := domain.AsError(err)
	if normalized.Code != domain.ErrCodeInternal {
		t.Fatalf("expected internal_error, got %q", normalized.Code)
	}
	if !strings.Contains(normalized.Message, "credential lookup failed") {
		t.Errorf("message = %q", normalized.Message)
	}
}

func TestAuthenticateBootstrapAdminKey(t *testing.T) {
	const adminKey = "syn_admin_bootstrap_key_value_1234567"
	store := newFakeStore()
	authenticator := New(Options{Store: store, AdminKey: adminKey})

	principal, err := authenticator.Authenticate(context.Background(), adminKey)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if !principal.System {
		t.Error("the bootstrap key yields the system principal")
	}
	if !principal.Has(domain.ScopeAdminTenants) {
		t.Error("the system principal holds every scope")
	}
	if principal.Label() != "system" {
		t.Errorf("label = %q", principal.Label())
	}
	if store.lookupCount() != 0 {
		t.Error("the bootstrap key must not require a database lookup")
	}

	// A near miss must not authenticate.
	if _, err := authenticator.Authenticate(context.Background(), adminKey+"x"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestAuthenticateAnonymous(t *testing.T) {
	authenticator, _, _ := newAuthenticator(t, Options{AllowAnonymousTenant: "dev"})

	principal, err := authenticator.AuthenticateAnonymous(context.Background())
	if err != nil {
		t.Fatalf("AuthenticateAnonymous: %v", err)
	}
	if principal.TenantID() != "dev" {
		t.Errorf("tenant = %q, want dev", principal.TenantID())
	}
	if principal.KeyID() != "" {
		t.Error("an anonymous principal has no key")
	}
	if principal.Has(domain.ScopeAdminKeys) {
		t.Error("an anonymous principal holds no scopes")
	}
}

func TestAuthenticateAnonymousDisabledByDefault(t *testing.T) {
	authenticator, _, _ := newAuthenticator(t, Options{})

	if _, err := authenticator.AuthenticateAnonymous(context.Background()); !errors.Is(err, ErrMissingCredentials) {
		t.Fatalf("expected ErrMissingCredentials, got %v", err)
	}
}

func TestInvalidateRemovesCachedCredential(t *testing.T) {
	authenticator, store, _ := newAuthenticator(t, Options{CacheTTL: time.Minute})
	token := "syn_live_" + strings.Repeat("j", 40)
	store.put(activeTenant("t-1"), activeKey("k-1", "t-1", token, string(domain.ScopeInference)))

	if _, err := authenticator.Authenticate(context.Background(), token); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if err := authenticator.Invalidate(context.Background(), token); err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if _, err := authenticator.Authenticate(context.Background(), token); err != nil {
		t.Fatalf("Authenticate after invalidation: %v", err)
	}

	// The cache was cleared, so revocation takes effect immediately rather than
	// after the TTL.
	if store.lookupCount() != 2 {
		t.Errorf("store lookups = %d, want 2 after invalidation", store.lookupCount())
	}
}

func TestCachedPrincipalExcludesTheKeyHash(t *testing.T) {
	tenant := activeTenant("t-1")
	key := activeKey("k-1", "t-1", "syn_live_"+strings.Repeat("k", 40), string(domain.ScopeInference))

	raw := encodeCachedPrincipal(&Principal{Tenant: tenant, APIKey: key})
	if raw == nil {
		t.Fatal("a valid principal should encode")
	}
	if strings.Contains(string(raw), key.KeyHash) {
		t.Error("the persisted key hash must never appear in the cache payload")
	}

	decoded, err := decodeCachedPrincipal(raw)
	if err != nil {
		t.Fatalf("decodeCachedPrincipal: %v", err)
	}
	if decoded.KeyID() != "k-1" || decoded.TenantID() != "t-1" {
		t.Errorf("round trip lost identity: %+v", decoded)
	}
}

func TestDecodeCachedPrincipalRejectsStaleEntries(t *testing.T) {
	if _, err := decodeCachedPrincipal([]byte(`{"v":99,"api_key":{"id":"k-1"}}`)); err == nil {
		t.Error("an entry with an unexpected version must be rejected")
	}
	if _, err := decodeCachedPrincipal([]byte(`not json`)); err == nil {
		t.Error("malformed JSON must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Key generation and scopes
// ---------------------------------------------------------------------------

func TestGenerateKeyIsUniqueAndHashed(t *testing.T) {
	seen := make(map[string]bool, 64)
	for i := 0; i < 64; i++ {
		generated, err := GenerateKey("syn_live_")
		if err != nil {
			t.Fatalf("GenerateKey: %v", err)
		}
		if seen[generated.Plaintext] {
			t.Fatal("GenerateKey produced a duplicate key")
		}
		seen[generated.Plaintext] = true

		if !strings.HasPrefix(generated.Plaintext, "syn_live_") {
			t.Errorf("plaintext %q lost its prefix", generated.Plaintext)
		}
		if generated.Hash != HashKey(generated.Plaintext) {
			t.Error("the stored hash must be the digest of the plaintext")
		}
		if strings.Contains(generated.Plaintext, generated.Hash) {
			t.Error("the hash must not be derivable from the display form")
		}
		if generated.Prefix != domain.DisplayPrefix(generated.Plaintext) {
			t.Errorf("prefix %q does not match the display form", generated.Prefix)
		}
		if len(generated.Plaintext) < 64 {
			t.Errorf("key %q is shorter than the entropy requirement", generated.Plaintext)
		}
	}
}

func TestGenerateKeyDefaultPrefix(t *testing.T) {
	generated, err := GenerateKey("")
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if !strings.HasPrefix(generated.Plaintext, "syn_live_") {
		t.Errorf("built-in prefix = %q", generated.Plaintext)
	}
}

func TestGenerateKeyFromBytesIsDeterministic(t *testing.T) {
	entropy := []byte{1, 2, 3, 4, 5, 6, 7, 8}

	first := GenerateKeyFromBytes("syn_test_", entropy)
	second := GenerateKeyFromBytes("syn_test_", entropy)
	if first.Plaintext != second.Plaintext {
		t.Error("the same entropy must mint the same key")
	}
	if first.Hash != HashKey(second.Plaintext) {
		t.Error("hash must match the plaintext")
	}
}

func TestPrincipalHasScopes(t *testing.T) {
	readOnly := &Principal{APIKey: &domain.APIKey{Scopes: []string{string(domain.ScopeInference)}}}
	if !readOnly.Has(domain.ScopeInference) {
		t.Error("the granted scope should be held")
	}
	if readOnly.Has(domain.ScopeAdminPolicies) {
		t.Error("an ungranted scope must not be held")
	}

	wildcard := &Principal{APIKey: &domain.APIKey{Scopes: []string{string(domain.ScopeAdminAll)}}}
	if !wildcard.Has(domain.ScopeAdminTenants) {
		t.Error("the wildcard scope grants everything")
	}

	var none *Principal
	if none.Has(domain.ScopeInference) {
		t.Error("a nil principal holds nothing")
	}
	if none.TenantID() != "" || none.KeyID() != "" {
		t.Error("a nil principal has no identifiers")
	}
	if none.Label() != "anonymous" {
		t.Errorf("nil label = %q", none.Label())
	}

	bare := &Principal{}
	if bare.Has(domain.ScopeInference) {
		t.Error("a principal with no key holds nothing")
	}
	if bare.Label() != "unknown" {
		t.Errorf("bare label = %q", bare.Label())
	}
}

func TestIsAuthenticationError(t *testing.T) {
	for _, err := range []error{
		ErrMissingCredentials,
		ErrInvalidCredentials,
		ErrKeyRevoked,
		ErrKeyExpired,
		ErrTenantInactive,
	} {
		if !IsAuthenticationError(err) {
			t.Errorf("%v should be classified as an authentication error", err)
		}
	}
	if IsAuthenticationError(ErrInsufficientScope(domain.ScopeAdminAll)) {
		t.Error("a scope failure is an authorization error, not an authentication one")
	}
	if IsAuthenticationError(errors.New("boom")) {
		t.Error("a foreign error is not an authentication error")
	}
}

func TestAuthErrorsAreNotFallbackEligible(t *testing.T) {
	// A gateway credential failure can never be fixed by trying another
	// provider, so these errors must never open the fallback path.
	for name, err := range map[string]*domain.Error{
		"missing":      ErrMissingCredentials,
		"invalid":      ErrInvalidCredentials,
		"revoked":      ErrKeyRevoked,
		"expired":      ErrKeyExpired,
		"tenant":       ErrTenantInactive,
		"insufficient": ErrInsufficientScope(domain.ScopeAdminAll),
	} {
		if err.FallbackEligible {
			t.Errorf("%s must not be fallback eligible", name)
		}
		if err.Retryable {
			t.Errorf("%s must not be retryable", name)
		}
		if err.Status != 401 && err.Status != 403 {
			t.Errorf("%s status = %d, want 401 or 403", name, err.Status)
		}
	}
}

func TestHeaderNameDefaults(t *testing.T) {
	authenticator, _, _ := newAuthenticator(t, Options{})
	if authenticator.HeaderName() != "Authorization" {
		t.Errorf("header = %q", authenticator.HeaderName())
	}
	if authenticator.RequireScope() {
		t.Error("scope enforcement must be opt-in")
	}

	custom, _, _ := newAuthenticator(t, Options{HeaderName: "X-API-Key", RequireScope: true})
	if custom.HeaderName() != "X-API-Key" {
		t.Errorf("header = %q", custom.HeaderName())
	}
	if !custom.RequireScope() {
		t.Error("scope enforcement should be enabled when asked for")
	}
}
