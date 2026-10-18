package admin

import (
	"context"
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/netguard"
)

// Validation caps keep admin writes sane and the error messages actionable.
const (
	maxNameLength   = 128
	maxURLLength    = 2048
	maxNotesLength  = 4096
	maxLabels       = 32
	maxCapabilities = 64
	maxAliases      = 32
	maxTargets      = 32
)

// validScopes is every scope the admin API may grant to a key.
var validScopes = map[string]bool{
	string(domain.ScopeInference):      true,
	string(domain.ScopeReadUsage):      true,
	string(domain.ScopeReadModels):     true,
	string(domain.ScopeAdminPolicies):  true,
	string(domain.ScopeAdminProviders): true,
	string(domain.ScopeAdminKeys):      true,
	string(domain.ScopeAdminTenants):   true,
	string(domain.ScopeAdminAll):       true,
}

// ValidateProvider checks a provider write before it reaches the database.
//
// The checks mirror the CHECK constraints plus the adapter registry's
// expectations, so a row that passes here both persists and builds.
func ValidateProvider(p *domain.Provider) error {
	if strings.TrimSpace(p.Name) == "" {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider name is required")
	}
	if len(p.Name) > maxNameLength {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider name exceeds 128 characters")
	}
	if !p.Kind.Valid() {
		return domain.Errorf(domain.ErrCodeInvalidRequest,
			"unknown provider kind %q: expected openai, anthropic, ollama, vllm or openai_compatible", p.Kind)
	}
	if strings.TrimSpace(p.BaseURL) == "" {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider base_url is required")
	}
	if len(p.BaseURL) > maxURLLength {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider base_url exceeds 2048 characters")
	}
	// Structural validation plus the provider address policy: loopback and
	// private ranges stay available for self-hosted runtimes, while link-local
	// and cloud-metadata destinations are refused. Provider probe, test and
	// sync-model endpoints all connect to this URL with the stored credential,
	// so a base_url is an SSRF sink if it is not constrained here.
	if _, err := netguard.ValidateProviderURL(context.Background(), p.BaseURL); err != nil {
		return domain.Errorf(domain.ErrCodeInvalidRequest,
			"provider base_url must be an http(s) URL, e.g. https://api.openai.com/v1 (%v)", err)
	}
	if p.AuthStyle != "" {
		switch p.AuthStyle {
		case domain.AuthBearer, domain.AuthHeader, domain.AuthNone:
		default:
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"unknown auth_style %q: expected bearer, header or none", p.AuthStyle)
		}
		if p.AuthStyle == domain.AuthHeader && strings.TrimSpace(p.HeaderName) == "" &&
			domain.DefaultAuthHeader(p.Kind) == "" {
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"header_name is required when auth_style is header and the kind has no default")
		}
	}
	if p.Status != "" {
		switch domain.Status(p.Status) {
		case domain.StatusActive, domain.StatusDegraded, domain.StatusDisabled, domain.StatusPending:
		default:
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"unknown provider status %q", p.Status)
		}
	}
	if p.Environment != "" && !p.Environment.Valid() {
		return domain.Errorf(domain.ErrCodeInvalidRequest,
			"unknown environment %q: expected production, internal, external or test", p.Environment)
	}
	if p.Weight < 0 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider weight must not be negative")
	}
	if p.TimeoutMS < 0 || p.MaxConcurrency < 0 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "timeout_ms and max_concurrency must not be negative")
	}
	if len(p.Notes) > maxNotesLength {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider notes exceed 4096 characters")
	}
	if len(p.Labels) > maxLabels {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider labels exceed 32 entries")
	}
	if len(p.Capabilities) > maxCapabilities {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider capabilities exceed 64 entries")
	}
	return nil
}

// NormalizeProvider applies creation defaults: active status, production
// environment and api ownership for admin writes.
func NormalizeProvider(p *domain.Provider) {
	if p.Status == "" {
		p.Status = domain.StatusActive
	}
	if p.Environment == "" {
		p.Environment = domain.EnvProduction
	}
	if p.ManagedBy == "" {
		p.ManagedBy = domain.ManagedByAPI
	}
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
}

// ValidateModel checks a model write.
func ValidateModel(m *domain.Model) error {
	if strings.TrimSpace(m.Name) == "" {
		return domain.NewError(domain.ErrCodeInvalidRequest, "model name is required")
	}
	if len(m.Name) > maxNameLength {
		return domain.NewError(domain.ErrCodeInvalidRequest, "model name exceeds 128 characters")
	}
	if strings.TrimSpace(m.ProviderID) == "" {
		return domain.NewError(domain.ErrCodeInvalidRequest, "provider_id is required")
	}
	if m.Status != "" {
		switch m.Status {
		case domain.ModelActive, domain.ModelDegraded, domain.ModelDeprecated, domain.ModelDisabled:
		default:
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"unknown model status %q", m.Status)
		}
	}
	if m.Environment != "" && !m.Environment.Valid() {
		return domain.Errorf(domain.ErrCodeInvalidRequest,
			"unknown environment %q: expected production, internal, external or test", m.Environment)
	}
	if m.ContextWindow < 0 || m.MaxOutputTokens < 0 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "context_window and max_output_tokens must not be negative")
	}
	if m.InputCostPerMillion < 0 || m.OutputCostPerMillion < 0 || m.CachedInputCostPerMillion < 0 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "model pricing must not be negative")
	}
	if m.QualityTier < 0 || m.QualityTier > 5 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "quality_tier must be between 0 and 5")
	}
	if m.RateLimitRPM < 0 || m.RateLimitTPM < 0 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "rate limits must not be negative")
	}
	if m.Priority < 0 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "model priority must not be negative")
	}
	if len(m.Aliases) > maxAliases {
		return domain.NewError(domain.ErrCodeInvalidRequest, "model aliases exceed 32 entries")
	}
	if len(m.Capabilities) > maxCapabilities {
		return domain.NewError(domain.ErrCodeInvalidRequest, "model capabilities exceed 64 entries")
	}
	return nil
}

// NormalizeModel applies creation defaults.
func NormalizeModel(m *domain.Model) {
	if m.Status == "" {
		m.Status = domain.ModelActive
	}
	if m.Environment == "" {
		m.Environment = domain.EnvProduction
	}
	if m.ManagedBy == "" {
		m.ManagedBy = domain.ManagedByAPI
	}
	if m.Priority == 0 {
		// Zero is the unset value; the registry default is 100, matching the
		// database default, so an operator omitting the field gets the same
		// row as the bootstrapper would write.
		m.Priority = 100
	}
	m.Name = strings.TrimSpace(m.Name)
}

// ValidateTenant checks a tenant write.
func ValidateTenant(t *domain.Tenant) error {
	if strings.TrimSpace(t.Slug) == "" {
		return domain.NewError(domain.ErrCodeInvalidRequest, "tenant slug is required")
	}
	if len(t.Slug) > 64 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "tenant slug exceeds 64 characters")
	}
	for _, r := range t.Slug {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"tenant slug must match ^[a-z0-9][a-z0-9_-]{1,62}$")
		}
	}
	if strings.TrimSpace(t.Name) == "" {
		return domain.NewError(domain.ErrCodeInvalidRequest, "tenant name is required")
	}
	if t.Status != "" {
		switch t.Status {
		case domain.StatusActive, domain.StatusDisabled, domain.StatusPending, domain.StatusRevoked:
		default:
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"unknown tenant status %q", t.Status)
		}
	}
	if len(t.Labels) > maxLabels {
		return domain.NewError(domain.ErrCodeInvalidRequest, "tenant labels exceed 32 entries")
	}
	return nil
}

// NormalizeKeyScopes validates scopes and defaults an empty set to inference
// only, the least-privilege choice.
func NormalizeKeyScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		return []string{string(domain.ScopeInference)}, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(scopes))
	for _, s := range scopes {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		if !validScopes[s] {
			return nil, domain.Errorf(domain.ErrCodeInvalidRequest,
				"unknown scope %q", s)
		}
		seen[s] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{string(domain.ScopeInference)}, nil
	}
	return out, nil
}

// ValidatePolicy checks a routing policy write at the structural level. Deep
// semantic validation (target references, match syntax) stays in the policy
// package, which owns those types.
func ValidatePolicy(p *domain.RoutingPolicy) error {
	if strings.TrimSpace(p.Name) == "" {
		return domain.NewError(domain.ErrCodeInvalidRequest, "policy name is required")
	}
	if len(p.Name) > maxNameLength {
		return domain.NewError(domain.ErrCodeInvalidRequest, "policy name exceeds 128 characters")
	}
	if len(p.Targets) == 0 {
		return domain.NewError(domain.ErrCodeInvalidRequest, "policy needs at least one target")
	}
	if len(p.Targets) > maxTargets {
		return domain.NewError(domain.ErrCodeInvalidRequest, "policy targets exceed 32 entries")
	}
	if p.Strategy != "" {
		switch p.Strategy {
		case domain.StrategyPriority, domain.StrategyWeighted, domain.StrategyLowestCost,
			domain.StrategyLowestLatency, domain.StrategyHighestQuality:
		default:
			return domain.Errorf(domain.ErrCodeInvalidRequest,
				"unknown strategy %q", p.Strategy)
		}
	}
	return nil
}
