// Package policy implements Synapass's policy engine: selecting which routing
// policy applies to a request and enforcing the numeric limits a policy declares.
//
// Matching and enforcement are separated from routing on purpose. Routing answers
// "given this policy, where does the request go?"; policy answers "which policy
// governs this request, and is it even allowed?". Keeping them apart means a
// change to matching rules cannot alter routing behaviour, and a policy can be
// unit-tested as a pure predicate.
package policy

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Repository supplies stored policies.
type Repository interface {
	// ListPolicies returns every policy, including disabled ones, because the
	// resolver must be able to report that a disabled policy matched.
	ListPolicies(ctx context.Context) ([]domain.RoutingPolicy, error)
	// GetPolicy returns a single policy by id.
	GetPolicy(ctx context.Context, id string) (*domain.RoutingPolicy, error)
}

// Resolver selects the policy that governs a request.
//
// Policies are cached in process and refreshed on a TTL. The TTL exists because
// the resolver runs on every request and a database round trip per request would
// put the system of record on the hot path for data that changes a few times a
// week. The stale window is bounded and configurable, and the admin API forces a
// refresh on write so an operator never has to wait for it.
type Resolver struct {
	source Repository
	ttl    time.Duration
	logger *slog.Logger
	now    func() time.Time

	mu       sync.RWMutex
	cache    []domain.RoutingPolicy
	loadedAt time.Time
	loaded   bool
}

// Option customizes a resolver.
type Option func(*Resolver)

// WithLogger attaches a logger.
func WithLogger(l *slog.Logger) Option {
	return func(r *Resolver) {
		if l != nil {
			r.logger = l
		}
	}
}

// WithClock overrides the clock. It exists for tests.
func WithClock(now func() time.Time) Option {
	return func(r *Resolver) {
		if now != nil {
			r.now = now
		}
	}
}

// NewResolver constructs a resolver.
func NewResolver(source Repository, ttl time.Duration, opts ...Option) *Resolver {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	r := &Resolver{
		source: source,
		ttl:    ttl,
		logger: slog.Default(),
		now:    time.Now,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Refresh reloads policies from the repository.
func (r *Resolver) Refresh(ctx context.Context) error {
	policies, err := r.source.ListPolicies(ctx)
	if err != nil {
		return domain.NewError(domain.ErrCodeInternal, "failed to load routing policies").Wrap(err)
	}
	for i := range policies {
		policies[i].Normalize()
	}

	r.mu.Lock()
	r.cache = policies
	r.loadedAt = r.now()
	r.loaded = true
	r.mu.Unlock()

	r.logger.Debug("routing policies refreshed", "count", len(policies))
	return nil
}

// cached returns the current cache, refreshing it when stale.
//
// A refresh failure is not fatal: the previous snapshot is served, because
// routing traffic with slightly stale policy is strictly better than refusing
// traffic because the database blinked. The failure is logged and the stale
// timestamp is left alone so the next request retries.
func (r *Resolver) cached(ctx context.Context) []domain.RoutingPolicy {
	r.mu.RLock()
	fresh := r.loaded && r.now().Sub(r.loadedAt) < r.ttl
	cached := r.cache
	r.mu.RUnlock()

	if fresh {
		return cached
	}

	if err := r.Refresh(ctx); err != nil {
		r.logger.Warn("routing policy refresh failed; serving the previous snapshot",
			"error", err)
		r.mu.RLock()
		defer r.mu.RUnlock()
		return r.cache
	}

	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cache
}

// Resolve returns the policy governing a request.
//
// Selection order, applied in sequence until a policy is chosen:
//
//	specificity desc  the narrowest match wins, so a per-key policy beats a
//	                  per-tenant policy which beats a global one
//	priority asc      the operator's explicit ordering breaks ties between
//	                  equally specific rules
//	version desc      a newer revision wins a remaining tie, so re-saving a
//	                  policy takes effect immediately
//
// The ordering is total, which means resolution never depends on row order from
// the database.
func (r *Resolver) Resolve(ctx context.Context, rc *domain.RequestContext) (*domain.RoutingPolicy, error) {
	if rc == nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "policy resolution requires a request context")
	}

	policies := r.cached(ctx)

	// A key pinned to a specific policy short-circuits matching entirely: the
	// operator made an explicit choice and nothing should override it.
	//
	// Pinning an id is not, however, a grant to use it. A policy that names
	// tenants, keys or endpoints only governs those subjects, so a caller that
	// knows another tenant's policy id must fall through to its own matching
	// rules rather than borrowing that policy's targets, budgets and cache
	// settings. UUIDs are unguessable but they appear in logs, exports and
	// support tickets, so secrecy is not the control.
	if rc.PolicyID != "" {
		if p := findByID(policies, rc.PolicyID); p != nil && p.Enabled && pinnedPolicyApplies(p, rc) {
			return p, nil
		}
		if p, err := r.source.GetPolicy(ctx, rc.PolicyID); err == nil && p != nil {
			p.Normalize()
			if p.Enabled && pinnedPolicyApplies(p, rc) {
				return p, nil
			}
		}
	}
	if rc.APIKey != nil && rc.APIKey.RoutingPolicyID != "" {
		if p := findByID(policies, rc.APIKey.RoutingPolicyID); p != nil && p.Enabled && pinnedPolicyApplies(p, rc) {
			return p, nil
		}
	}

	matches := make([]domain.RoutingPolicy, 0, len(policies))
	for _, p := range policies {
		if !p.Enabled {
			continue
		}
		if Matches(&p, rc) {
			matches = append(matches, p)
		}
	}
	if len(matches) == 0 {
		// Returning nil (rather than an error) asks the routing engine to
		// synthesize its default policy, which keeps "no policy configured" a
		// working state rather than a misconfiguration.
		return nil, nil
	}

	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if sa, sb := a.Match.Specificity(), b.Match.Specificity(); sa != sb {
			return sa > sb
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.Version != b.Version {
			return a.Version > b.Version
		}
		return a.Name < b.Name
	})

	selected := matches[0]
	return &selected, nil
}

// List returns the cached policies, refreshing if needed.
func (r *Resolver) List(ctx context.Context) ([]domain.RoutingPolicy, error) {
	policies := r.cached(ctx)
	out := make([]domain.RoutingPolicy, len(policies))
	copy(out, policies)
	return out, nil
}

// Get returns a single policy by id, preferring the cache.
func (r *Resolver) Get(ctx context.Context, id string) (*domain.RoutingPolicy, error) {
	if p := findByID(r.cached(ctx), id); p != nil {
		return p, nil
	}
	p, err := r.source.GetPolicy(ctx, id)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, domain.NewError(domain.ErrCodeNotFound, "routing policy not found")
	}
	p.Normalize()
	return p, nil
}

// pinnedPolicyApplies reports whether a policy referenced by id may govern this
// request.
//
// Every populated scope on the policy must include the caller: a policy scoped
// to tenant A is not usable by tenant B even when B knows its id, and the same
// holds for key and endpoint grants. A policy with no subject scoping is a
// genuinely global policy and remains pinnable by anyone, which is the
// documented behaviour for operator-wide defaults.
func pinnedPolicyApplies(p *domain.RoutingPolicy, rc *domain.RequestContext) bool {
	if p == nil || rc == nil {
		return false
	}
	if len(p.Match.TenantIDs) > 0 {
		if rc.Tenant == nil || !containsString(p.Match.TenantIDs, rc.Tenant.ID) {
			return false
		}
	}
	if len(p.Match.APIKeyIDs) > 0 {
		if rc.APIKey == nil || !containsString(p.Match.APIKeyIDs, rc.APIKey.ID) {
			return false
		}
	}
	if len(p.Match.EndpointIDs) > 0 {
		if rc.EndpointID == "" || !containsString(p.Match.EndpointIDs, rc.EndpointID) {
			return false
		}
	}
	return true
}

// findByID looks up a policy in a slice.
func findByID(policies []domain.RoutingPolicy, id string) *domain.RoutingPolicy {
	for i := range policies {
		if policies[i].ID == id {
			return &policies[i]
		}
	}
	return nil
}

// Matches reports whether a policy applies to a request.
//
// Every populated field on the policy's match block must be satisfied; empty
// fields are wildcards. This is a pure function of its two arguments, which is
// what makes the matching rules testable without a database or a request.
func Matches(p *domain.RoutingPolicy, rc *domain.RequestContext) bool {
	if p == nil || rc == nil {
		return false
	}
	m := p.Match

	// Tenant scoping. A policy bound to a tenant applies only to that tenant.
	if len(m.TenantIDs) > 0 {
		if rc.Tenant == nil || !containsString(m.TenantIDs, rc.Tenant.ID) {
			return false
		}
		// A tenant with its own policies is not also governed by global ones for
		// the same class of traffic; specificity ordering already prefers the
		// tenant policy, so no extra filtering is required here.
	}
	if len(m.APIKeyIDs) > 0 {
		if rc.APIKey == nil || !containsString(m.APIKeyIDs, rc.APIKey.ID) {
			return false
		}
	}
	if len(m.RequestTypes) > 0 && !containsRequestType(m.RequestTypes, rc.RequestType) {
		return false
	}
	if !m.MatchesModel(rc.RequestedModel) {
		return false
	}
	if m.Streaming != nil && *m.Streaming != rc.Stream {
		return false
	}
	if m.MinPromptTokens > 0 && rc.PromptTokens < m.MinPromptTokens {
		return false
	}
	if m.MaxPromptTokens > 0 && rc.PromptTokens > m.MaxPromptTokens {
		return false
	}
	if len(m.RequiredCapabilities) > 0 {
		available := rc.CapabilitySet()
		for _, required := range m.RequiredCapabilities {
			if !available.Contains(required) {
				return false
			}
		}
	}
	// Phase 2: endpoint scope.
	if len(m.EndpointIDs) > 0 {
		if rc.EndpointID == "" || !containsString(m.EndpointIDs, rc.EndpointID) {
			return false
		}
	}
	// Phase 2: task class.
	if len(m.TaskTypes) > 0 {
		matched := false
		current := rc.Task.Task
		if current == "" {
			current = "chat"
		}
		for _, want := range m.TaskTypes {
			if want == current {
				matched = true
				break
			}
			for _, sec := range rc.Task.Secondary {
				if want == sec {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return false
		}
	}
	// Phase 2: batch vs interactive.
	if m.Batch != nil && *m.Batch != rc.Batch {
		return false
	}
	// Phase 2: region constraint on match (policy-level region affinity).
	if len(m.Regions) > 0 {
		if rc.Region == "" {
			return false
		}
		found := false
		for _, r := range m.Regions {
			if equalFold(r, rc.Region) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	// Phase 2: sensitivity constraint.
	if len(m.DataSensitivity) > 0 {
		if len(rc.DataSensitivity) == 0 {
			return false
		}
		found := false
		for _, want := range m.DataSensitivity {
			for _, have := range rc.DataSensitivity {
				if equalFold(want, have) {
					found = true
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func containsRequestType(values []domain.RequestType, want domain.RequestType) bool {
	for _, v := range values {
		if v == want {
			return true
		}
		// Accept the short shorthands operators write in configuration
		// ("chat", "completions", "embeddings") for the canonical API surface
		// names ("chat.completions", ...). Without this, a policy declaring
		// `request_types: [chat]` silently never matches, and the gateway falls
		// back to synthesized defaults while the dashboard shows a policy that
		// looks active. Existing stored rows keep working unchanged.
		if normalizeRequestType(v) == want {
			return true
		}
	}
	return false
}

// normalizeRequestType maps operator shorthands onto canonical request types.
func normalizeRequestType(v domain.RequestType) domain.RequestType {
	switch v {
	case "chat":
		return domain.RequestTypeChatCompletion
	case "completions":
		return domain.RequestTypeCompletion
	case "embeddings":
		return domain.RequestTypeEmbedding
	default:
		return v
	}
}

func equalFold(a, b string) bool {
	if len(a) != len(b) {
		// Fast path still needs case folding; fall through to manual compare.
	}
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	// ASCII case-fold is sufficient for region/sensitivity labels.
	if len(a) != len(b) {
		return false
	}
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

// Describe renders a policy for logs and audit events, omitting the target list
// so the line stays short.
func Describe(p *domain.RoutingPolicy) string {
	if p == nil {
		return "(none)"
	}
	var b strings.Builder
	b.WriteString(p.Name)
	if p.ID == "" {
		b.WriteString(" (synthesized)")
	}
	b.WriteString(" strategy=")
	b.WriteString(string(p.Strategy))
	b.WriteString(" targets=")
	b.WriteString(itoa(len(p.Targets)))
	b.WriteString(" version=")
	b.WriteString(itoa(p.Version))
	return b.String()
}

// itoa is a tiny integer formatter used to avoid importing strconv in the hot
// matching path.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
