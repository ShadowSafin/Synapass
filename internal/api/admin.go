package api

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/shadowsafin/synapass/internal/auth"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/storage"
)

// storageFilter is a local alias so the handler signatures stay readable.
type storageFilter = storage.QueryFilter

// handleAdminOverview serves the dashboard landing payload.
func (s *Server) handleAdminOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	window, err := parseTimeRange(r, 24*time.Hour)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	filter := storageFilter{
		TenantID: r.URL.Query().Get("tenant_id"),
		From:     window.From,
		To:       window.To,
	}

	overview := domain.Overview{Window: window}

	summary, err := s.repos.Usage.Summary(ctx, filter)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	overview.Summary = *summary

	// The previous window is fetched so the dashboard can render deltas without a
	// second round trip. A failure here is tolerated: a missing comparison is a
	// cosmetic problem, not a reason to fail the whole page.
	previous := window.Previous()
	if prevSummary, perr := s.repos.Usage.Summary(ctx, storageFilter{
		TenantID: filter.TenantID, From: previous.From, To: previous.To,
	}); perr == nil {
		overview.PreviousSummary = *prevSummary
	}

	series, err := s.repos.Usage.Series(ctx, filter, window.Interval)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	overview.Series = series

	if providers, perr := s.repos.Usage.ByProvider(ctx, filter, 10); perr == nil {
		overview.Providers = providers
	}
	if models, merr := s.repos.Usage.ByModel(ctx, filter, 10); merr == nil {
		overview.Models = models
	}
	if s.health != nil {
		overview.ProviderHealth = s.health.Snapshot()
	}

	writeJSON(w, http.StatusOK, overview)
}

// systemResponse is the body of GET /admin/v1/system.
type systemResponse struct {
	Version    any               `json:"version"`
	Uptime     string            `json:"uptime"`
	Config     any               `json:"config"`
	Providers  []providerSummary `json:"providers"`
	Components map[string]any    `json:"components"`
	Runtime    map[string]any    `json:"runtime"`
}

// providerSummary is a provider's configuration plus current health.
type providerSummary struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Kind         string   `json:"kind"`
	BaseURL      string   `json:"base_url"`
	Status       string   `json:"status"`
	Capabilities []string `json:"capabilities"`
	Priority     int      `json:"priority"`
	Weight       int      `json:"weight"`
	Region       string   `json:"region"`
	Notes        string   `json:"notes,omitempty"`
	Environment  string   `json:"environment,omitempty"`
	ManagedBy    string   `json:"managed_by,omitempty"`
	// HasCredential reports whether an encrypted credential is stored. The
	// secret itself is never serialized.
	HasCredential bool                   `json:"has_credential"`
	ModelCount    int                    `json:"model_count"`
	Health        *domain.ProviderHealth `json:"health,omitempty"`
	// AdapterReady reports whether a working adapter exists. It is the difference
	// between "configured" and "usable".
	AdapterReady bool              `json:"adapter_ready"`
	Labels       map[string]string `json:"labels,omitempty"`
}

// handleAdminSystem reports platform-wide diagnostics.
func (s *Server) handleAdminSystem(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	body := systemResponse{
		Version: s.version,
		Uptime:  s.uptime().Round(time.Second).String(),
		// Secrets are redacted: this endpoint is reachable by any admin-scoped key,
		// so it must never echo a credential.
		Config:     s.config.Redacted(),
		Components: map[string]any{},
		Runtime: map[string]any{
			"goroutines": runtime.NumGoroutine(),
			"go_version": runtime.Version(),
			"num_cpu":    runtime.NumCPU(),
			"gomaxprocs": runtime.GOMAXPROCS(0),
		},
	}

	if s.postgres != nil {
		body.Components["postgres"] = s.postgres.Stat()
	}
	if s.redis != nil {
		body.Components["redis"] = s.redis.Stat()
	}
	if s.clickhouse != nil {
		body.Components["clickhouse"] = s.clickhouse.Stats()
	}
	if s.nats != nil {
		body.Components["nats"] = s.nats.Stats()
	}
	if s.recorder != nil {
		body.Components["telemetry_pipeline"] = s.recorder.Stats()
	}
	if s.health != nil {
		body.Components["provider_health"] = s.health.Snapshot()
	}

	providers, err := s.listProviderSummaries(ctx)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	body.Providers = providers

	writeJSON(w, http.StatusOK, body)
}

// listProviderSummaries combines provider configuration with live health.
func (s *Server) listProviderSummaries(ctx context.Context) ([]providerSummary, error) {
	if s.repos == nil || s.repos.Providers == nil {
		return nil, nil
	}

	providers, err := s.repos.Providers.List(ctx)
	if err != nil {
		return nil, err
	}

	// Model counts come from one registry read rather than a query per provider.
	// Credential presence likewise comes from a single query.
	modelCounts := map[string]int{}
	if s.repos.Models != nil {
		if models, merr := s.repos.Models.List(ctx); merr == nil {
			for _, m := range models {
				modelCounts[m.ProviderID]++
			}
		}
	}
	withCredential := map[string]bool{}
	if s.repos.Credentials != nil {
		if ids, cerr := s.repos.Credentials.PresentIDs(ctx); cerr == nil {
			withCredential = ids
		}
	}

	out := make([]providerSummary, 0, len(providers))
	for _, p := range providers {
		capabilities := make([]string, 0, len(p.Capabilities))
		for _, c := range p.Capabilities {
			capabilities = append(capabilities, string(c))
		}

		summary := providerSummary{
			ID:            p.ID,
			Name:          p.Name,
			Kind:          string(p.Kind),
			BaseURL:       p.BaseURL,
			Status:        string(p.Status),
			Capabilities:  capabilities,
			Priority:      p.Priority,
			Weight:        p.Weight,
			Region:        p.Region,
			Notes:         p.Notes,
			Environment:   string(p.Environment),
			ManagedBy:     string(p.ManagedBy),
			HasCredential: withCredential[p.ID],
			ModelCount:    modelCounts[p.ID],
			Labels:        p.Labels,
		}

		if s.adapters != nil {
			_, summary.AdapterReady = s.adapters.Resolve(p.ID)
		}
		if s.health != nil {
			if health, ok := s.health.Health(p.ID); ok {
				summary.Health = &health
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

// handleAdminListProviders serves GET /admin/v1/providers.
func (s *Server) handleAdminListProviders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()

	providers, err := s.listProviderSummaries(ctx)
	if err != nil {
		writeError(w, err, metaFromContext(requestContext(ctx), nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

// handleAdminProviderHealth serves GET /admin/v1/providers/health.
func (s *Server) handleAdminProviderHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.health == nil {
		writeJSON(w, http.StatusOK, map[string]any{"health": []domain.ProviderHealth{}})
		return
	}

	// Snapshots are attached when a provider id is given, so the dashboard can show
	// a health history without a second endpoint.
	health := s.health.Snapshot()
	providerID := r.URL.Query().Get("provider_id")
	if providerID != "" && s.repos != nil && s.repos.Snapshots != nil {
		snapshots, err := s.repos.Snapshots.ListRecent(ctx, providerID, parseIntParam(r, "limit", 100))
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"health": health, "snapshots": snapshots})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"health": health})
}

// handleAdminProbeProvider runs an on-demand health probe.
func (s *Server) handleAdminProbeProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 20*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	id := chiURLParam(r, "id")
	if s.adapters == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "no providers are configured"),
			metaFromContext(rc, nil))
		return
	}

	adapter, ok := s.adapters.Resolve(id)
	if !ok {
		writeError(w, domain.Errorf(domain.ErrCodeNotFound, "provider %q is not configured", id),
			metaFromContext(rc, nil))
		return
	}

	health := adapter.HealthCheck(ctx)
	if s.health != nil {
		s.health.RecordProbe(health)
	}
	if s.metrics != nil {
		s.metrics.SetProviderHealth(health.ProviderName, string(health.State))
	}
	if s.nats != nil {
		s.nats.PublishProviderHealth(&health)
	}

	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceProvider, adapter.Name(), map[string]any{
		"action": "probe",
		"state":  string(health.State),
	}, nil)

	writeJSON(w, http.StatusOK, health)
}

// handleAdminListModels serves GET /admin/v1/models.
func (s *Server) handleAdminListModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Models == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the model registry is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	models, err := s.repos.Models.List(ctx)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	// The admin view includes disabled models, because an operator needs to see
	// what is configured as well as what is serving.
	providerFilter := r.URL.Query().Get("provider")
	if providerFilter == "" {
		writeJSON(w, http.StatusOK, map[string]any{"models": models})
		return
	}

	filtered := make([]domain.Model, 0, len(models))
	for _, m := range models {
		if m.ProviderName == providerFilter || m.ProviderID == providerFilter {
			filtered = append(filtered, m)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": filtered})
}

// handleAdminListPolicies serves GET /admin/v1/policies.
func (s *Server) handleAdminListPolicies(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	policies, err := s.policies.List(ctx)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"policies": policies})
}

// handleAdminGetPolicy serves GET /admin/v1/policies/{id}.
func (s *Server) handleAdminGetPolicy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	policy, err := s.policies.Get(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, policy)
}

// handleAdminUpsertPolicy serves PUT /admin/v1/policies.
func (s *Server) handleAdminUpsertPolicy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Policies == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the policy store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	var policy domain.RoutingPolicy
	if err := decodeJSONBody(r, 1<<20, &policy); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if policy.Name == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "'name' is required"),
			metaFromContext(rc, nil))
		return
	}
	if len(policy.Targets) == 0 {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"'targets' must contain at least one entry"), metaFromContext(rc, nil))
		return
	}
	if policy.Strategy != "" && !policy.Strategy.Valid() {
		writeError(w, domain.Errorf(domain.ErrCodeInvalidRequest,
			"'strategy' %q is not recognized", policy.Strategy), metaFromContext(rc, nil))
		return
	}

	if p := principal(ctx); p != nil {
		policy.CreatedBy = p.Label()
	}
	// An upsert through the API takes ownership of the row, so the
	// bootstrapper will not revert it on the next restart.
	policy.ManagedBy = domain.ManagedByAPI

	saved, err := s.repos.Policies.Upsert(ctx, &policy)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	// Refresh the resolver so the change takes effect immediately rather than after
	// the cache TTL. An operator who saves a policy expects it to apply.
	if err := s.policies.Refresh(ctx); err != nil {
		s.logger.Warn("failed to refresh policies after a write", "error", err)
	}

	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceRoutingPolicy, saved.ID, nil, map[string]any{
		"name":     saved.Name,
		"strategy": string(saved.Strategy),
		"version":  saved.Version,
	})

	writeJSON(w, http.StatusOK, saved)
}

// handleAdminDeletePolicy serves DELETE /admin/v1/policies/{id}.
func (s *Server) handleAdminDeletePolicy(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Policies == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the policy store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	if err := s.repos.Policies.Delete(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if err := s.policies.Refresh(ctx); err != nil {
		s.logger.Warn("failed to refresh policies after a delete", "error", err)
	}

	s.audit(ctx, rc, domain.AuditDelete, domain.ResourceRoutingPolicy, id, nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleAdminReloadPolicies forces a policy cache refresh.
func (s *Server) handleAdminReloadPolicies(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()

	if err := s.policies.Refresh(ctx); err != nil {
		writeError(w, err, metaFromContext(requestContext(ctx), nil))
		return
	}
	policies, _ := s.policies.List(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"reloaded": len(policies)})
}

// handleAdminListTenants serves GET /admin/v1/tenants.
func (s *Server) handleAdminListTenants(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Tenants == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the tenant store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	tenants, err := s.repos.Tenants.List(ctx)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tenants": tenants})
}

// handleAdminListKeys serves GET /admin/v1/keys.
func (s *Server) handleAdminListKeys(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.APIKeys == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the key store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	tenantID := r.URL.Query().Get("tenant_id")
	if tenantID == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"'tenant_id' is required; listing every key across tenants is not permitted"),
			metaFromContext(rc, nil))
		return
	}

	keys, err := s.repos.APIKeys.ListByTenant(ctx, tenantID)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// createKeyRequest is the body of POST /admin/v1/keys.
type createKeyRequest struct {
	TenantID        string   `json:"tenant_id"`
	Name            string   `json:"name"`
	Scopes          []string `json:"scopes"`
	RoutingPolicyID string   `json:"routing_policy_id,omitempty"`
	// ExpiresInHours of zero means the key does not expire.
	ExpiresInHours int `json:"expires_in_hours,omitempty"`
}

// handleAdminCreateKey mints a new API key.
//
// The plaintext is returned exactly once and never stored, which is the only safe
// way to handle a credential: a leaked database cannot yield a usable key.
func (s *Server) handleAdminCreateKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.APIKeys == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the key store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	var body createKeyRequest
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.TenantID == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "'tenant_id' is required"),
			metaFromContext(rc, nil))
		return
	}

	// Validate the tenant up front. The insert would also reject an unknown id
	// via its foreign key, but that surfaces as an opaque internal error; a 404
	// says what actually went wrong. Keys are never global, so this is also the
	// point where "which tenant" is enforced.
	if s.repos.Tenants == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the tenant store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	tenant, err := s.repos.Tenants.GetByID(ctx, body.TenantID)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if tenant == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "the selected tenant does not exist"),
			metaFromContext(rc, nil))
		return
	}

	generated, err := auth.GenerateKey(s.config.Auth.KeyPrefix)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	// Validate the scope set and confine it to what the caller holds. A key with
	// no explicit scopes defaults to inference only, which is the least-privilege
	// choice for the overwhelmingly common case. An invalid or over-broad grant
	// must be rejected before anything is written: a stored key is a credential
	// that outlives the request that created it.
	scopes, err := normalizeGrantedScopes(ctx, body.Scopes)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	key := &domain.APIKey{
		ID:              domain.NewID(),
		TenantID:        body.TenantID,
		Name:            body.Name,
		Prefix:          generated.Prefix,
		KeyHash:         generated.Hash,
		Scopes:          scopes,
		Status:          domain.APIKeyActive,
		RoutingPolicyID: body.RoutingPolicyID,
		CreatedAt:       domain.Now(),
	}
	if p := principal(ctx); p != nil {
		key.CreatedBy = p.Label()
	}
	if body.ExpiresInHours > 0 {
		expiry := domain.Now().Add(time.Duration(body.ExpiresInHours) * time.Hour)
		key.ExpiresAt = &expiry
	}

	if err := s.repos.APIKeys.Create(ctx, key); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceAPIKey, key.ID, nil, map[string]any{
		"name":      key.Name,
		"prefix":    key.Prefix,
		"scopes":    key.Scopes,
		"tenant_id": key.TenantID,
	})

	writeJSON(w, http.StatusCreated, map[string]any{
		"key": key,
		// The plaintext is in this response and nowhere else.
		"plaintext": generated.Plaintext,
		"warning":   "store this key now; it cannot be retrieved again",
	})
}

// handleAdminRevokeKey serves DELETE /admin/v1/keys/{id}.
func (s *Server) handleAdminRevokeKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.APIKeys == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the key store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")

	// The hash is read before revocation so the credential cache entry can be
	// invalidated; revocation clears the hash, which would make the cache key
	// unrecoverable.
	key, err := s.repos.APIKeys.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if key == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "api key not found"),
			metaFromContext(rc, nil))
		return
	}

	if err := s.repos.APIKeys.Revoke(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	// Dropping the cached credential makes revocation take effect immediately
	// instead of after the cache TTL.
	if err := s.auth.InvalidateHash(ctx, key.KeyHash); err != nil {
		s.logger.Warn("failed to invalidate a cached credential", "key_id", id, "error", err)
	}

	s.audit(ctx, rc, domain.AuditRevoke, domain.ResourceAPIKey, id, map[string]any{
		"prefix": key.Prefix, "name": key.Name,
	}, nil)

	writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

// handleAdminUsageSummary serves GET /admin/v1/usage/summary.
func (s *Server) handleAdminUsageSummary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	window, err := parseTimeRange(r, 24*time.Hour)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	filter := encodeQueryFilter(r, "")
	filter.From, filter.To = window.From, window.To

	summary, err := s.repos.Usage.Summary(ctx, filter)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	providers, _ := s.repos.Usage.ByProvider(ctx, filter, 20)
	models, _ := s.repos.Usage.ByModel(ctx, filter, 20)

	writeJSON(w, http.StatusOK, map[string]any{
		"window":    window,
		"summary":   summary,
		"providers": providers,
		"models":    models,
	})
}

// handleAdminUsageSeries serves GET /admin/v1/usage/series.
func (s *Server) handleAdminUsageSeries(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 20*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	window, err := parseTimeRange(r, 24*time.Hour)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	filter := encodeQueryFilter(r, "")
	filter.From, filter.To = window.From, window.To

	series, err := s.repos.Usage.Series(ctx, filter, window.Interval)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"window": window, "series": series})
}

// handleAdminListRequests serves GET /admin/v1/requests.
func (s *Server) handleAdminListRequests(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Logs == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the request log store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	window, err := parseTimeRange(r, 24*time.Hour)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	filter := encodeQueryFilter(r, "")
	filter.From, filter.To = window.From, window.To

	logs, err := s.repos.Logs.List(ctx, filter)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"window":   window,
		"requests": logs,
		"limit":    filter.Limit,
		"offset":   filter.Offset,
	})
}

// handleAdminGetRequest serves GET /admin/v1/requests/{requestID}.
func (s *Server) handleAdminGetRequest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Logs == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the request log store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	requestID := chiURLParam(r, "requestID")
	entry, err := s.repos.Logs.GetByRequestID(ctx, requestID)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if entry == nil {
		writeError(w, domain.Errorf(domain.ErrCodeNotFound, "request %q was not found", requestID),
			metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

// handleAdminListErrors serves GET /admin/v1/errors.
func (s *Server) handleAdminListErrors(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Logs == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the request log store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	window, err := parseTimeRange(r, 24*time.Hour)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	filter := encodeQueryFilter(r, "")
	filter.From, filter.To = window.From, window.To
	// An error view defaults to 5xx unless the caller asks for a wider net.
	if filter.StatusMin == 0 {
		filter.StatusMin = 400
	}

	logs, err := s.repos.Logs.List(ctx, filter)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	// A per-code rollup is what makes the errors page actionable: it answers "what
	// is failing most?" without the operator counting rows.
	byCode := map[string]int{}
	for _, entry := range logs {
		code := string(entry.ErrorCode)
		if code == "" {
			code = "unknown"
		}
		byCode[code]++
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"window":  window,
		"errors":  logs,
		"by_code": byCode,
		"total":   len(logs),
	})
}

// handleAdminListAudit serves GET /admin/v1/audit.
func (s *Server) handleAdminListAudit(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Audit == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the audit store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	events, err := s.repos.Audit.List(ctx,
		r.URL.Query().Get("tenant_id"),
		domain.AuditResource(r.URL.Query().Get("resource")),
		parseIntParam(r, "limit", 100),
		parseIntParam(r, "offset", 0))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

// handleAdminListBudgets serves GET /admin/v1/budgets.
func (s *Server) handleAdminListBudgets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Budgets == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the budget store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	budgets, err := s.repos.Budgets.ListByTenant(ctx, r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"budgets": budgets})
}

// audit writes an audit event.
//
// Audit failures are logged but never returned to the caller: refusing a completed
// administrative action because its audit row could not be written would leave the
// system in a state that matches neither the operator's intent nor the audit log.
func (s *Server) audit(
	ctx context.Context,
	rc *domain.RequestContext,
	action domain.AuditAction,
	resource domain.AuditResource,
	resourceID string,
	before, after map[string]any,
) {
	if s.repos == nil || s.repos.Audit == nil {
		return
	}

	event := &domain.AuditEvent{
		Action:     action,
		Resource:   resource,
		ResourceID: resourceID,
		Before:     before,
		After:      after,
		CreatedAt:  domain.Now(),
	}
	if rc != nil {
		event.TenantID = rc.TenantID()
		event.ActorKeyID = rc.APIKeyID()
		event.ActorIP = rc.ClientIP
		event.RequestID = rc.RequestID
	}
	if p := principal(ctx); p != nil {
		event.ActorLabel = p.Label()
	}

	if err := s.repos.Audit.Insert(ctx, event); err != nil {
		s.logger.Warn("failed to write an audit event",
			"action", action, "resource", resource, "resource_id", resourceID, "error", err)
		return
	}
	// Publishing to NATS lets external systems consume the audit trail without
	// polling the API.
	if s.nats != nil {
		s.nats.PublishAudit(event)
	}
}
