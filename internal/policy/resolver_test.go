package policy

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// fakeRepository is an in-memory policy store.
type fakeRepository struct {
	mu       sync.Mutex
	policies []domain.RoutingPolicy
	loads    int
	err      error
}

func (f *fakeRepository) ListPolicies(context.Context) ([]domain.RoutingPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads++
	if f.err != nil {
		return nil, f.err
	}
	out := make([]domain.RoutingPolicy, len(f.policies))
	copy(out, f.policies)
	return out, nil
}

func (f *fakeRepository) GetPolicy(_ context.Context, id string) (*domain.RoutingPolicy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.policies {
		if f.policies[i].ID == id {
			policy := f.policies[i]
			return &policy, nil
		}
	}
	return nil, nil
}

func (f *fakeRepository) loadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

func TestResolverSelectsMostSpecificPolicy(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{
			ID: "global", Name: "global", Enabled: true, Priority: 1,
			Match: domain.PolicyMatch{},
		},
		{
			ID: "model", Name: "model", Enabled: true, Priority: 1,
			Match: domain.PolicyMatch{Models: []string{"gpt-4o"}},
		},
		{
			ID: "key", Name: "key", Enabled: true, Priority: 5,
			Match: domain.PolicyMatch{Models: []string{"gpt-4o"}, APIKeyIDs: []string{"key-1"}},
		},
	}}

	resolver := NewResolver(repo, time.Minute)

	rc := &domain.RequestContext{
		RequestedModel: "gpt-4o",
		APIKey:         &domain.APIKey{ID: "key-1"},
	}

	selected, err := resolver.Resolve(context.Background(), rc)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selected == nil {
		t.Fatal("expected a policy to be selected")
	}
	// Specificity must beat priority: the key-scoped policy has a worse priority
	// number but a narrower match.
	if selected.ID != "key" {
		t.Fatalf("selected %q, want the key-scoped policy", selected.ID)
	}
}

func TestResolverFallsBackToWildcard(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{ID: "global", Name: "global", Enabled: true, Match: domain.PolicyMatch{}},
	}}
	resolver := NewResolver(repo, time.Minute)

	selected, err := resolver.Resolve(context.Background(), &domain.RequestContext{RequestedModel: "anything"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selected == nil || selected.ID != "global" {
		t.Fatalf("selected %v, want the wildcard policy", selected)
	}
}

func TestResolverReturnsNilWhenNothingMatches(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{
			ID: "model-only", Name: "model-only", Enabled: true,
			Match: domain.PolicyMatch{Models: []string{"claude-3*"}},
		},
	}}
	resolver := NewResolver(repo, time.Minute)

	selected, err := resolver.Resolve(context.Background(), &domain.RequestContext{RequestedModel: "gpt-4o"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	// Returning nil asks the routing engine to synthesize its default, which keeps
	// "no policy configured" a working state rather than an error.
	if selected != nil {
		t.Fatalf("selected %q, want nil so the engine can synthesize a default", selected.ID)
	}
}

func TestResolverIgnoresDisabledPolicies(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{
			ID: "disabled", Name: "disabled", Enabled: false,
			Match: domain.PolicyMatch{Models: []string{"gpt-4o"}},
		},
	}}
	resolver := NewResolver(repo, time.Minute)

	selected, err := resolver.Resolve(context.Background(), &domain.RequestContext{RequestedModel: "gpt-4o"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selected != nil {
		t.Fatalf("a disabled policy must not be selected, got %q", selected.ID)
	}
}

func TestResolverHonoursPinnedPolicy(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{ID: "pinned", Name: "pinned", Enabled: true, Match: domain.PolicyMatch{Models: []string{"never-matches"}}},
		{ID: "wildcard", Name: "wildcard", Enabled: true, Match: domain.PolicyMatch{}},
	}}
	resolver := NewResolver(repo, time.Minute)

	// A key pinned to a policy bypasses matching entirely: the operator made an
	// explicit choice.
	rc := &domain.RequestContext{
		RequestedModel: "gpt-4o",
		APIKey:         &domain.APIKey{ID: "k", RoutingPolicyID: "pinned"},
	}

	selected, err := resolver.Resolve(context.Background(), rc)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selected == nil || selected.ID != "pinned" {
		t.Fatalf("selected %v, want the pinned policy", selected)
	}
}

func TestResolverRejectsPinnedPolicyFromAnotherTenant(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{
			ID: "tenant-a", Name: "tenant-a", Enabled: true,
			Match: domain.PolicyMatch{TenantIDs: []string{"t-a"}},
		},
		{ID: "wildcard", Name: "wildcard", Enabled: true, Match: domain.PolicyMatch{}},
	}}
	resolver := NewResolver(repo, time.Minute)

	rc := &domain.RequestContext{
		RequestedModel: "gpt-4o",
		Tenant:         &domain.Tenant{ID: "t-b"},
		PolicyID:       "tenant-a",
	}

	selected, err := resolver.Resolve(context.Background(), rc)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selected == nil || selected.ID != "wildcard" {
		t.Fatalf("selected %v, want the caller's own policy rather than another tenant's", selected)
	}
}

func TestResolverRejectsPinnedPolicyScopedToAnotherKeyOrEndpoint(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{ID: "key-a", Name: "key-a", Enabled: true, Match: domain.PolicyMatch{APIKeyIDs: []string{"k-a"}}},
		{ID: "endpoint-a", Name: "endpoint-a", Enabled: true, Match: domain.PolicyMatch{EndpointIDs: []string{"e-a"}}},
		{ID: "wildcard", Name: "wildcard", Enabled: true, Match: domain.PolicyMatch{}},
	}}
	resolver := NewResolver(repo, time.Minute)

	for _, pin := range []string{"key-a", "endpoint-a"} {
		rc := &domain.RequestContext{
			RequestedModel: "gpt-4o",
			Tenant:         &domain.Tenant{ID: "t-b"},
			APIKey:         &domain.APIKey{ID: "k-b"},
			EndpointID:     "e-b",
			PolicyID:       pin,
		}
		selected, err := resolver.Resolve(context.Background(), rc)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", pin, err)
		}
		if selected == nil || selected.ID != "wildcard" {
			t.Fatalf("pinned %s selected %v, want the wildcard fallback", pin, selected)
		}
	}
}

func TestResolverSkipsDisabledPinnedPolicyFromStore(t *testing.T) {
	// The cached list is empty, so the explicit-id path falls through to the
	// repository; a disabled row there must not become routable just because a
	// caller named it.
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{ID: "disabled", Name: "disabled", Enabled: false, Match: domain.PolicyMatch{}},
	}}
	resolver := NewResolver(repo, time.Minute)

	rc := &domain.RequestContext{RequestedModel: "gpt-4o", PolicyID: "disabled"}
	selected, err := resolver.Resolve(context.Background(), rc)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if selected != nil {
		t.Fatalf("a disabled pinned policy must not be selected, got %q", selected.ID)
	}
}

func TestResolverCachesWithinTTL(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{ID: "p", Name: "p", Enabled: true, Match: domain.PolicyMatch{}},
	}}

	now := time.Now()
	resolver := NewResolver(repo, time.Minute, WithClock(func() time.Time { return now }))

	rc := &domain.RequestContext{RequestedModel: "gpt-4o"}
	for i := 0; i < 5; i++ {
		if _, err := resolver.Resolve(context.Background(), rc); err != nil {
			t.Fatalf("Resolve: %v", err)
		}
	}
	if got := repo.loadCount(); got != 1 {
		t.Fatalf("the repository was read %d times within the TTL, want 1", got)
	}

	// Advancing past the TTL must trigger exactly one more read.
	now = now.Add(2 * time.Minute)
	if _, err := resolver.Resolve(context.Background(), rc); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := repo.loadCount(); got != 2 {
		t.Fatalf("the repository was read %d times after the TTL, want 2", got)
	}
}

func TestResolverServesStaleCacheOnRefreshFailure(t *testing.T) {
	repo := &fakeRepository{policies: []domain.RoutingPolicy{
		{ID: "p", Name: "p", Enabled: true, Match: domain.PolicyMatch{}},
	}}

	now := time.Now()
	resolver := NewResolver(repo, time.Millisecond, WithClock(func() time.Time { return now }))

	rc := &domain.RequestContext{RequestedModel: "gpt-4o"}
	if _, err := resolver.Resolve(context.Background(), rc); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// The store now fails, and the TTL has elapsed.
	repo.mu.Lock()
	repo.err = errors.New("database is down")
	repo.mu.Unlock()
	now = now.Add(time.Hour)

	selected, err := resolver.Resolve(context.Background(), rc)
	if err != nil {
		t.Fatalf("a refresh failure must not surface to the caller: %v", err)
	}
	// Serving a slightly stale policy is strictly better than refusing traffic
	// because the database blinked.
	if selected == nil || selected.ID != "p" {
		t.Fatalf("selected %v, want the cached policy", selected)
	}
}

func TestMatchesIsAPurePredicate(t *testing.T) {
	streaming := true
	policy := &domain.RoutingPolicy{
		Enabled: true,
		Match: domain.PolicyMatch{
			Models:               []string{"gpt-4*"},
			RequestTypes:         []domain.RequestType{domain.RequestTypeChatCompletion},
			TenantIDs:            []string{"tenant-1"},
			MinPromptTokens:      10,
			MaxPromptTokens:      1000,
			RequiredCapabilities: []domain.Capability{domain.CapTools},
			Streaming:            &streaming,
		},
	}

	base := func() *domain.RequestContext {
		return &domain.RequestContext{
			RequestedModel:       "gpt-4o",
			RequestType:          domain.RequestTypeChatCompletion,
			Tenant:               &domain.Tenant{ID: "tenant-1"},
			PromptTokens:         100,
			RequiredCapabilities: []domain.Capability{domain.CapTools},
			Stream:               true,
		}
	}

	if !Matches(policy, base()) {
		t.Fatal("a request satisfying every constraint must match")
	}

	tests := []struct {
		name   string
		mutate func(*domain.RequestContext)
	}{
		{"wrong model", func(rc *domain.RequestContext) { rc.RequestedModel = "claude-3" }},
		{"wrong tenant", func(rc *domain.RequestContext) { rc.Tenant = &domain.Tenant{ID: "other"} }},
		{"no tenant", func(rc *domain.RequestContext) { rc.Tenant = nil }},
		{"prompt too small", func(rc *domain.RequestContext) { rc.PromptTokens = 1 }},
		{"prompt too large", func(rc *domain.RequestContext) { rc.PromptTokens = 5000 }},
		{"missing capability", func(rc *domain.RequestContext) { rc.RequiredCapabilities = nil }},
		{"not streaming", func(rc *domain.RequestContext) { rc.Stream = false }},
		{"wrong request type", func(rc *domain.RequestContext) { rc.RequestType = domain.RequestTypeEmbedding }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rc := base()
			tc.mutate(rc)
			if Matches(policy, rc) {
				t.Fatal("the policy must not match")
			}
		})
	}
}

func TestBudgetPeriodKeys(t *testing.T) {
	reference := time.Date(2026, time.March, 15, 14, 30, 0, 0, time.UTC)

	if got := PeriodKey(domain.BudgetDaily, reference); got != "d:2026-03-15" {
		t.Fatalf("daily period key = %q, want d:2026-03-15", got)
	}
	if got := PeriodKey(domain.BudgetMonthly, reference); got != "m:2026-03" {
		t.Fatalf("monthly period key = %q, want m:2026-03", got)
	}
	// Daily and monthly keys must never collide, or a charge would be counted
	// against the wrong ceiling.
	if PeriodKey(domain.BudgetDaily, reference) == PeriodKey(domain.BudgetMonthly, reference) {
		t.Fatal("daily and monthly period keys must be distinct")
	}

	// Reset boundaries must land on the start of the following period, because the
	// dashboard renders "resets in N hours" from this value.
	daily := ResetAt(domain.BudgetDaily, reference)
	if !daily.Equal(time.Date(2026, time.March, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily reset = %s, want 2026-03-16T00:00:00Z", daily)
	}
	monthly := ResetAt(domain.BudgetMonthly, reference)
	if !monthly.Equal(time.Date(2026, time.April, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("monthly reset = %s, want 2026-04-01T00:00:00Z", monthly)
	}
}

// fakeCounterStore is an in-memory counter implementation.
type fakeCounterStore struct {
	mu     sync.Mutex
	spend  map[string]float64
	limits map[string]int
	err    error
}

func newFakeCounterStore() *fakeCounterStore {
	return &fakeCounterStore{spend: map[string]float64{}, limits: map[string]int{}}
}

// spendKey mirrors the real store, which namespaces a counter by period label as
// well as by scope. Omitting the period here would make a daily and a monthly
// charge collide and hide exactly the kind of double-counting bug this suite should
// catch.
func spendKey(key, periodKey string) string {
	return key + "|" + periodKey
}

func (f *fakeCounterStore) IncrementSpend(_ context.Context, key string, amount float64, periodKey string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	k := spendKey(key, periodKey)
	f.spend[k] += amount
	return f.spend[k], nil
}

func (f *fakeCounterStore) GetSpend(_ context.Context, key, periodKey string) (float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return 0, f.err
	}
	return f.spend[spendKey(key, periodKey)], nil
}

func (f *fakeCounterStore) AllowRequest(_ context.Context, key string, limit int, _ time.Duration) (bool, int, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, 0, time.Time{}, f.err
	}
	count := f.limits[key]
	if count >= limit {
		return false, 0, time.Now().Add(time.Minute), nil
	}
	f.limits[key] = count + 1
	return true, limit - count - 1, time.Now().Add(time.Minute), nil
}

func TestEnforcerBlocksRequestsOverBudget(t *testing.T) {
	store := newFakeCounterStore()
	enforcer := NewEnforcer(store)

	rc := &domain.RequestContext{
		RequestID: "req-1",
		Tenant:    &domain.Tenant{ID: "tenant-1"},
		APIKey:    &domain.APIKey{ID: "key-1"},
	}
	policy := &domain.RoutingPolicy{
		Limits: domain.PolicyLimits{DailyBudgetUSD: 1.00},
	}

	if err := enforcer.CheckPreflight(context.Background(), rc, policy); err != nil {
		t.Fatalf("the first request must be allowed: %v", err)
	}

	// Simulate a day of spending.
	if err := enforcer.Charge(context.Background(), rc, domain.Cost{USD: 0.95}); err != nil {
		t.Fatalf("Charge: %v", err)
	}
	if err := enforcer.CheckPreflight(context.Background(), rc, policy); err != nil {
		t.Fatalf("a request within budget must be allowed: %v", err)
	}

	if err := enforcer.Charge(context.Background(), rc, domain.Cost{USD: 0.10}); err != nil {
		t.Fatalf("Charge: %v", err)
	}
	err := enforcer.CheckPreflight(context.Background(), rc, policy)
	if err == nil {
		t.Fatal("a request over budget must be rejected")
	}
	if got := domain.AsError(err).Code; got != domain.ErrCodeQuotaExceeded {
		t.Fatalf("Code = %q, want %q", got, domain.ErrCodeQuotaExceeded)
	}
	if status := domain.AsError(err).HTTPStatus(); status != 402 {
		t.Fatalf("HTTPStatus() = %d, want 402", status)
	}
}

func TestEnforcerBlocksRequestsOverRateLimit(t *testing.T) {
	store := newFakeCounterStore()
	enforcer := NewEnforcer(store)

	rc := &domain.RequestContext{
		RequestID: "req-1",
		Tenant:    &domain.Tenant{ID: "tenant-1"},
		APIKey:    &domain.APIKey{ID: "key-1"},
	}
	policy := &domain.RoutingPolicy{Limits: domain.PolicyLimits{RequestsPerMinute: 2}}

	for i := 0; i < 2; i++ {
		if err := enforcer.CheckPreflight(context.Background(), rc, policy); err != nil {
			t.Fatalf("request %d must be allowed: %v", i+1, err)
		}
	}

	err := enforcer.CheckPreflight(context.Background(), rc, policy)
	if err == nil {
		t.Fatal("the request over the rate limit must be rejected")
	}
	if got := domain.AsError(err).Code; got != domain.ErrCodeRateLimited {
		t.Fatalf("Code = %q, want %q", got, domain.ErrCodeRateLimited)
	}
}

func TestEnforcerFailsOpenWhenStoreIsUnavailable(t *testing.T) {
	store := newFakeCounterStore()
	store.err = errors.New("redis is down")
	enforcer := NewEnforcer(store)

	rc := &domain.RequestContext{
		RequestID: "req-1",
		Tenant:    &domain.Tenant{ID: "tenant-1"},
	}
	policy := &domain.RoutingPolicy{Limits: domain.PolicyLimits{
		RequestsPerMinute: 1,
		DailyBudgetUSD:    1,
	}}

	// A limiter that cannot reach its store must not become an outage: refusing
	// traffic because a cache is down would make the cache a hard dependency.
	if err := enforcer.CheckPreflight(context.Background(), rc, policy); err != nil {
		t.Fatalf("a limiter store failure must fail open, got: %v", err)
	}
}

func TestEnforcerDisabledDoesNotBlock(t *testing.T) {
	store := newFakeCounterStore()
	store.spend[spendKey("tenant:tenant-1", PeriodKey(domain.BudgetDaily, time.Now()))] = 1000
	enforcer := NewEnforcer(store, WithEnforcementEnabled(false))

	rc := &domain.RequestContext{RequestID: "req-1", Tenant: &domain.Tenant{ID: "tenant-1"}}
	policy := &domain.RoutingPolicy{Limits: domain.PolicyLimits{DailyBudgetUSD: 1}}

	if err := enforcer.CheckPreflight(context.Background(), rc, policy); err != nil {
		t.Fatalf("enforcement is disabled, so nothing must be blocked: %v", err)
	}
}
