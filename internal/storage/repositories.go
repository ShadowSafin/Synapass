package storage

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Repositories bundles the durable stores so a service can be constructed with a
// single dependency instead of a dozen.
type Repositories struct {
	Tenants   *TenantRepository
	APIKeys   *APIKeyRepository
	Providers *ProviderRepository
	Models    *ModelRepository
	Policies  *PolicyRepository
	Usage     *UsageRepository
	Logs      *RequestLogRepository
	Audit     *AuditRepository
	Budgets   *BudgetRepository
	Snapshots *ProviderSnapshotRepository
	Settings  *SettingsRepository
	// Phase 2 stores.
	Replay    *ReplayRepository
	Feedback  *FeedbackRepository
	Overrides *OverrideRepository
	Endpoints *EndpointRepository
	// Captured prompts behind request ids, for offline replay.
	Payloads *PayloadRepository
	// Phase 3 stores.
	Credentials *CredentialRepository
	TestResults *TestResultRepository
	// Phase 4 stores.
	Tools        *ToolRepository
	ToolPolicies *ToolPolicyRepository
	Invocations  *ToolInvocationRepository
	AgentRuns    *AgentRunRepository
	// Phase 5 stores.
	CachePolicies      *CachePolicyRepository
	CacheInvalidations *CacheInvalidationRepository
	CacheEntries       *CacheEntryRepository
	// Tunnel sessions: temporary public exposure audit trail.
	Tunnels *TunnelSessionRepository
	// Console operator identity and sessions. Separate from APIKeys because a
	// password is stretched with Argon2id and the account is lockable, whereas a
	// key is high-entropy material verified by digest lookup.
	DashboardUsers    *DashboardUserRepository
	DashboardSessions *DashboardSessionRepository
	// Cost intelligence: versioned pricing, fired budget alerts, anomalies.
	Pricing      *PricingRepository
	BudgetAlerts *BudgetAlertRepository
	Anomalies    *AnomalyRepository
	// Autonomous Benchmark Engine (disabled by default; inert until enabled).
	BenchmarkJobs          *BenchmarkJobRepository
	BenchmarkDatasets      *BenchmarkDatasetRepository
	BenchmarkPrompts       *BenchmarkPromptRepository
	BenchmarkRuns          *BenchmarkRunRepository
	BenchmarkResults       *BenchmarkResultRepository
	BenchmarkReports       *BenchmarkReportRepository
	BenchmarkNotifications *BenchmarkNotificationRepository
	BenchmarkWorkerLogs    *BenchmarkWorkerLogRepository
}

// NewRepositories builds every repository over a pool.
func NewRepositories(pool *pgxpool.Pool, logger *slog.Logger) *Repositories {
	if logger == nil {
		logger = slog.Default()
	}
	return &Repositories{
		Tenants:   &TenantRepository{pool: pool, logger: logger},
		APIKeys:   &APIKeyRepository{pool: pool, logger: logger},
		Providers: &ProviderRepository{pool: pool, logger: logger},
		Models:    &ModelRepository{pool: pool, logger: logger},
		Policies:  &PolicyRepository{pool: pool, logger: logger},
		Usage:     &UsageRepository{pool: pool, logger: logger},
		Logs:      &RequestLogRepository{pool: pool, logger: logger},
		Audit:     &AuditRepository{pool: pool, logger: logger},
		Budgets:   &BudgetRepository{pool: pool, logger: logger},
		Snapshots: &ProviderSnapshotRepository{pool: pool, logger: logger},
		Settings:  &SettingsRepository{pool: pool, logger: logger},
		Replay:    NewReplayRepository(pool, logger),
		Feedback:  NewFeedbackRepository(pool, logger),
		Overrides: NewOverrideRepository(pool, logger),
		Endpoints: NewEndpointRepository(pool, logger),
		Payloads:  NewPayloadRepository(pool, logger),

		Credentials: NewCredentialRepository(pool, logger),
		TestResults: NewTestResultRepository(pool, logger),

		Tools:        NewToolRepository(pool, logger),
		ToolPolicies: NewToolPolicyRepository(pool, logger),
		Invocations:  NewToolInvocationRepository(pool, logger),
		AgentRuns:    NewAgentRunRepository(pool, logger),

		CachePolicies:      NewCachePolicyRepository(pool, logger),
		CacheInvalidations: NewCacheInvalidationRepository(pool, logger),
		CacheEntries:       NewCacheEntryRepository(pool, logger),

		Tunnels: NewTunnelSessionRepository(pool, logger),

		DashboardUsers:    NewDashboardUserRepository(pool, logger),
		DashboardSessions: NewDashboardSessionRepository(pool, logger),

		Pricing:      NewPricingRepository(pool, logger),
		BudgetAlerts: NewBudgetAlertRepository(pool, logger),
		Anomalies:    NewAnomalyRepository(pool, logger),

		BenchmarkJobs:          NewBenchmarkJobRepository(pool, logger),
		BenchmarkDatasets:      NewBenchmarkDatasetRepository(pool, logger),
		BenchmarkPrompts:       NewBenchmarkPromptRepository(pool, logger),
		BenchmarkRuns:          NewBenchmarkRunRepository(pool, logger),
		BenchmarkResults:       NewBenchmarkResultRepository(pool, logger),
		BenchmarkReports:       NewBenchmarkReportRepository(pool, logger),
		BenchmarkNotifications: NewBenchmarkNotificationRepository(pool, logger),
		BenchmarkWorkerLogs:    NewBenchmarkWorkerLogRepository(pool, logger),
	}
}

// wrapDBError converts a driver error into a normalized one, keeping the cause.
func wrapDBError(message string, err error) *domain.Error {
	return domain.NewError(domain.ErrCodeInternal, message).Wrap(err)
}

// isNotFound reports whether err is a missing-row error.
func isNotFound(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

// TenantRepository stores tenants.
type TenantRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

const tenantColumns = `id, slug, name, status, plan, COALESCE(default_routing_policy_id::text, ''),
	labels, created_at, updated_at`

// UpsertBySlug creates or updates a tenant identified by its slug.
//
// The bootstrap path uses an upsert because a restart must be idempotent: the
// same configuration file applied twice must not fail. Slug is the natural key
// because it is what an operator writes in configuration.
func (r *TenantRepository) UpsertBySlug(ctx context.Context, t *domain.Tenant) (*domain.Tenant, error) {
	if t.Labels == nil {
		t.Labels = map[string]string{}
	}
	labels, err := json.Marshal(t.Labels)
	if err != nil {
		return nil, wrapDBError("encode tenant labels", err)
	}

	row := r.pool.QueryRow(ctx, `
		INSERT INTO tenants (id, slug, name, status, plan, labels)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (slug) DO UPDATE
			SET name = EXCLUDED.name,
			    status = EXCLUDED.status,
			    plan = EXCLUDED.plan,
			    labels = EXCLUDED.labels,
			    updated_at = now()
		RETURNING `+tenantColumns,
		defaultID(t.ID), t.Slug, t.Name, statusOrDefault(string(t.Status)), t.Plan, labels)

	out, err := scanTenant(row)
	if err != nil {
		return nil, wrapDBError("upsert tenant "+t.Slug, err)
	}
	return out, nil
}

// GetByID returns a tenant by identifier.
func (r *TenantRepository) GetByID(ctx context.Context, id string) (*domain.Tenant, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE id = $1`, id)
	t, err := scanTenant(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load tenant", err)
	}
	return t, nil
}

// GetBySlug returns a tenant by slug.
func (r *TenantRepository) GetBySlug(ctx context.Context, slug string) (*domain.Tenant, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE slug = $1`, slug)
	t, err := scanTenant(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load tenant by slug", err)
	}
	return t, nil
}

// List returns tenants ordered by name.
func (r *TenantRepository) List(ctx context.Context) ([]domain.Tenant, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+tenantColumns+` FROM tenants ORDER BY name`)
	if err != nil {
		return nil, wrapDBError("list tenants", err)
	}
	defer rows.Close()

	var out []domain.Tenant
	for rows.Next() {
		t, err := scanTenant(rows)
		if err != nil {
			return nil, wrapDBError("scan tenant", err)
		}
		out = append(out, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list tenants", err)
	}
	return out, nil
}

// Update modifies a tenant's mutable fields.
func (r *TenantRepository) Update(ctx context.Context, t *domain.Tenant) error {
	labels, err := json.Marshal(orEmptyMap(t.Labels))
	if err != nil {
		return wrapDBError("encode tenant labels", err)
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE tenants
		   SET name = $2, status = $3, plan = $4, labels = $5, updated_at = now()
		 WHERE id = $1`,
		t.ID, t.Name, string(t.Status), t.Plan, labels)
	if err != nil {
		return wrapDBError("update tenant", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "tenant not found")
	}
	return nil
}

// SetDefaultPolicy points a tenant at a default routing policy.
func (r *TenantRepository) SetDefaultPolicy(ctx context.Context, tenantID, policyID string) error {
	var policyArg any
	if policyID != "" {
		policyArg = policyID
	}
	_, err := r.pool.Exec(ctx,
		`UPDATE tenants SET default_routing_policy_id = $2, updated_at = now() WHERE id = $1`,
		tenantID, policyArg)
	if err != nil {
		return wrapDBError("set tenant default policy", err)
	}
	return nil
}

// Delete removes a tenant. API keys cascade; the caller guards against
// deleting a tenant that still owns active keys unless forced.
func (r *TenantRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
	if err != nil {
		return wrapDBError("delete tenant", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "tenant not found")
	}
	return nil
}

// scanTenant reads a tenant row.
func scanTenant(row pgx.Row) (*domain.Tenant, error) {
	var (
		t          domain.Tenant
		status     string
		defaultPol string
		labels     []byte
	)
	if err := row.Scan(&t.ID, &t.Slug, &t.Name, &status, &t.Plan, &defaultPol,
		&labels, &t.CreatedAt, &t.UpdatedAt); err != nil {
		return nil, err
	}
	t.Status = domain.Status(status)
	t.DefaultRoutingPolicyID = defaultPol
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &t.Labels); err != nil {
			// Corrupt metadata must not make a tenant unusable; it is dropped and
			// the tenant continues to serve traffic.
			t.Labels = nil
		}
	}
	return &t, nil
}

// ---------------------------------------------------------------------------
// API keys
// ---------------------------------------------------------------------------

// APIKeyRepository stores credentials. It implements auth.KeyStore.
type APIKeyRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

const apiKeyColumns = `id, tenant_id, name, prefix, key_hash, scopes, status,
	expires_at, last_used_at, COALESCE(routing_policy_id::text, ''), created_at, revoked_at, created_by`

// Create inserts a key.
func (r *APIKeyRepository) Create(ctx context.Context, key *domain.APIKey) error {
	if key.ID == "" {
		key.ID = domain.NewID()
	}
	if key.CreatedAt.IsZero() {
		key.CreatedAt = domain.Now()
	}
	scopes := key.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO api_keys (id, tenant_id, name, prefix, key_hash, scopes, status,
			expires_at, routing_policy_id, created_at, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		key.ID, key.TenantID, key.Name, key.Prefix, key.KeyHash, scopes,
		statusOrDefault(string(key.Status)), key.ExpiresAt, nullableUUID(key.RoutingPolicyID),
		key.CreatedAt, key.CreatedBy)
	if err != nil {
		return wrapDBError("create api key", err)
	}
	return nil
}

// LookupKey implements auth.KeyStore: it finds a key and its tenant by hash.
//
// The join is expressed as two statements rather than one, because a key whose
// tenant row is missing would otherwise disappear entirely from a LEFT JOIN and
// produce a confusing "invalid key" for what is really a data integrity problem.
func (r *APIKeyRepository) LookupKey(ctx context.Context, keyHash string) (*domain.APIKey, *domain.Tenant, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE key_hash = $1`, keyHash)
	key, err := scanAPIKey(row)
	if isNotFound(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, wrapDBError("load api key", err)
	}

	tenantRow := r.pool.QueryRow(ctx, `SELECT `+tenantColumns+` FROM tenants WHERE id = $1`, key.TenantID)
	tenant, err := scanTenant(tenantRow)
	if isNotFound(err) {
		return key, nil, nil
	}
	if err != nil {
		return nil, nil, wrapDBError("load tenant for api key", err)
	}
	return key, tenant, nil
}

// LookupByPrefix implements auth.KeyStore.
func (r *APIKeyRepository) LookupByPrefix(ctx context.Context, prefix string) ([]domain.APIKey, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE prefix = $1 ORDER BY created_at DESC`, prefix)
	if err != nil {
		return nil, wrapDBError("lookup api keys by prefix", err)
	}
	defer rows.Close()
	return collectAPIKeys(rows)
}

// TouchKey records last-used telemetry.
//
// The write is deliberately non-blocking at the call site and is skipped when the
// recorded timestamp is recent, so a hot key does not generate a write per
// request. The guard is a single cheap UPDATE ... WHERE rather than a read-modify
// -write, so it stays correct under concurrency.
func (r *APIKeyRepository) TouchKey(ctx context.Context, keyID string) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE api_keys
		   SET last_used_at = now()
		 WHERE id = $1
		   AND (last_used_at IS NULL OR last_used_at < now() - INTERVAL '60 seconds')`, keyID)
	if err != nil {
		return wrapDBError("touch api key", err)
	}
	return nil
}

// ListByTenant returns a tenant's keys.
func (r *APIKeyRepository) ListByTenant(ctx context.Context, tenantID string) ([]domain.APIKey, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT `+apiKeyColumns+` FROM api_keys WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, wrapDBError("list api keys", err)
	}
	defer rows.Close()
	return collectAPIKeys(rows)
}

// GetByID returns a key by identifier.
func (r *APIKeyRepository) GetByID(ctx context.Context, id string) (*domain.APIKey, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+apiKeyColumns+` FROM api_keys WHERE id = $1`, id)
	key, err := scanAPIKey(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load api key", err)
	}
	return key, nil
}

// Revoke marks a key revoked and clears its hash.
//
// Clearing the hash is what makes revocation irreversible and immediately
// effective: even if a cache entry survives, the stored digest no longer matches
// any presented token. The prefix is retained so support can still identify the
// key.
func (r *APIKeyRepository) Revoke(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE api_keys
		   SET status = 'revoked', revoked_at = now(), key_hash = 'revoked:' || id::text
		 WHERE id = $1 AND status = 'active'`, id)
	if err != nil {
		return wrapDBError("revoke api key", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "active api key not found")
	}
	return nil
}

// Update rewrites a key's editable metadata: name, scopes, expiry and pinned
// policy. The secret itself is never editable; rotation mints a new one.
func (r *APIKeyRepository) Update(ctx context.Context, key *domain.APIKey) (*domain.APIKey, error) {
	scopes := key.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	tag, err := r.pool.Exec(ctx, `
		UPDATE api_keys
		   SET name = $2, scopes = $3, expires_at = $4, routing_policy_id = $5
		 WHERE id = $1 AND status = 'active'`,
		key.ID, key.Name, scopes, key.ExpiresAt, nullableUUID(key.RoutingPolicyID))
	if err != nil {
		return nil, wrapDBError("update api key", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, domain.NewError(domain.ErrCodeNotFound, "active api key not found")
	}
	return r.GetByID(ctx, key.ID)
}

// Rotate swaps a key's secret while keeping its identity, name, scopes and
// tenant. The old plaintext stops working as soon as the caller invalidates
// its cache entry; rotation is how a leaked key is contained without
// re-provisioning every client configuration that references the key id.
func (r *APIKeyRepository) Rotate(ctx context.Context, id, prefix, keyHash string) (*domain.APIKey, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE api_keys
		   SET prefix = $2, key_hash = $3, revoked_at = NULL
		 WHERE id = $1 AND status = 'active'`, id, prefix, keyHash)
	if err != nil {
		return nil, wrapDBError("rotate api key", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, domain.NewError(domain.ErrCodeNotFound, "active api key not found")
	}
	return r.GetByID(ctx, id)
}

func scanAPIKey(row pgx.Row) (*domain.APIKey, error) {
	var (
		k          domain.APIKey
		status     string
		routingPol string
	)
	if err := row.Scan(&k.ID, &k.TenantID, &k.Name, &k.Prefix, &k.KeyHash, &k.Scopes,
		&status, &k.ExpiresAt, &k.LastUsedAt, &routingPol, &k.CreatedAt, &k.RevokedAt,
		&k.CreatedBy); err != nil {
		return nil, err
	}
	k.Status = domain.APIKeyStatus(status)
	k.RoutingPolicyID = routingPol
	return &k, nil
}

func collectAPIKeys(rows pgx.Rows) ([]domain.APIKey, error) {
	var out []domain.APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, wrapDBError("scan api key", err)
		}
		out = append(out, *k)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list api keys", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

// ProviderRepository stores provider configuration.
type ProviderRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

const providerColumns = `id, name, kind, base_url, api_key_env, auth_style, header_name,
	headers, organization, project, capabilities, status, weight, priority, timeout_ms,
	max_concurrency, region, notes, environment, managed_by, labels, created_at, updated_at`

// Upsert creates or updates a provider identified by name.
//
// managed_by is persisted on every write: the bootstrapper stamps bootstrap,
// the admin API stamps api, so a restart never silently reverts an operator
// edit (and vice versa the seeder skips api-managed rows before calling this).
func (r *ProviderRepository) Upsert(ctx context.Context, p *domain.Provider) (*domain.Provider, error) {
	if p.ID == "" {
		p.ID = domain.NewID()
	}
	headers, err := json.Marshal(orEmptyMap(p.Headers))
	if err != nil {
		return nil, wrapDBError("encode provider headers", err)
	}
	labels, err := json.Marshal(orEmptyMap(p.Labels))
	if err != nil {
		return nil, wrapDBError("encode provider labels", err)
	}
	caps := p.Capabilities
	if caps == nil {
		caps = []domain.Capability{}
	}

	row := r.pool.QueryRow(ctx, `
		INSERT INTO providers (id, name, kind, base_url, api_key_env, auth_style, header_name,
			headers, organization, project, capabilities, status, weight, priority, timeout_ms,
			max_concurrency, region, notes, environment, managed_by, labels)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
		ON CONFLICT (name) DO UPDATE
			SET kind = EXCLUDED.kind,
			    base_url = EXCLUDED.base_url,
			    api_key_env = EXCLUDED.api_key_env,
			    auth_style = EXCLUDED.auth_style,
			    header_name = EXCLUDED.header_name,
			    headers = EXCLUDED.headers,
			    organization = EXCLUDED.organization,
			    project = EXCLUDED.project,
			    capabilities = EXCLUDED.capabilities,
			    status = EXCLUDED.status,
			    weight = EXCLUDED.weight,
			    priority = EXCLUDED.priority,
			    timeout_ms = EXCLUDED.timeout_ms,
			    max_concurrency = EXCLUDED.max_concurrency,
			    region = EXCLUDED.region,
			    notes = EXCLUDED.notes,
			    environment = EXCLUDED.environment,
			    managed_by = EXCLUDED.managed_by,
			    labels = EXCLUDED.labels,
			    updated_at = now()
		RETURNING `+providerColumns,
		p.ID, p.Name, string(p.Kind), p.BaseURL, p.APIKeyEnv, authStyleOrDefault(p.Kind, string(p.AuthStyle)),
		authHeaderOrDefault(p.Kind, p.HeaderName), headers, p.Organization, p.Project, capabilityStrings(caps),
		statusOrDefault(string(p.Status)), defaultInt(p.Weight, 1), p.Priority, p.TimeoutMS,
		p.MaxConcurrency, p.Region, p.Notes, envOrDefault(string(p.Environment)),
		managedByOrDefault(string(p.ManagedBy)), labels)

	out, err := scanProvider(row)
	if err != nil {
		return nil, wrapDBError("upsert provider "+p.Name, err)
	}
	return out, nil
}

// List returns every provider.
func (r *ProviderRepository) List(ctx context.Context) ([]domain.Provider, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+providerColumns+` FROM providers ORDER BY priority, name`)
	if err != nil {
		return nil, wrapDBError("list providers", err)
	}
	defer rows.Close()

	var out []domain.Provider
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, wrapDBError("scan provider", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list providers", err)
	}
	return out, nil
}

// GetByID returns a provider by identifier.
func (r *ProviderRepository) GetByID(ctx context.Context, id string) (*domain.Provider, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+providerColumns+` FROM providers WHERE id = $1`, id)
	p, err := scanProvider(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load provider", err)
	}
	return p, nil
}

// GetByName returns a provider by name.
func (r *ProviderRepository) GetByName(ctx context.Context, name string) (*domain.Provider, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+providerColumns+` FROM providers WHERE name = $1`, name)
	p, err := scanProvider(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load provider", err)
	}
	return p, nil
}

// SetStatus updates a provider's status, used by the health loop and the admin API.
func (r *ProviderRepository) SetStatus(ctx context.Context, id string, status domain.Status) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE providers SET status = $2, updated_at = now() WHERE id = $1`, id, string(status))
	if err != nil {
		return wrapDBError("set provider status", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "provider not found")
	}
	return nil
}

// Delete removes a provider and, by cascade, its models.
func (r *ProviderRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM providers WHERE id = $1`, id)
	if err != nil {
		return wrapDBError("delete provider", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "provider not found")
	}
	return nil
}

func scanProvider(row pgx.Row) (*domain.Provider, error) {
	var (
		p            domain.Provider
		kind         string
		authStyle    string
		status       string
		environment  string
		managedBy    string
		headers      []byte
		labels       []byte
		capabilities []string
	)
	if err := row.Scan(&p.ID, &p.Name, &kind, &p.BaseURL, &p.APIKeyEnv, &authStyle,
		&p.HeaderName, &headers, &p.Organization, &p.Project, &capabilities, &status,
		&p.Weight, &p.Priority, &p.TimeoutMS, &p.MaxConcurrency, &p.Region, &p.Notes,
		&environment, &managedBy, &labels, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Kind = domain.ProviderKind(kind)
	p.AuthStyle = domain.AuthStyle(authStyle)
	p.Status = domain.Status(status)
	p.Environment = domain.Environment(environment)
	p.ManagedBy = domain.ManagedBy(managedBy)
	p.Capabilities = toCapabilities(capabilities)
	if len(headers) > 0 {
		_ = json.Unmarshal(headers, &p.Headers)
	}
	if len(labels) > 0 {
		_ = json.Unmarshal(labels, &p.Labels)
	}
	return &p, nil
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

// ModelRepository stores the model registry.
type ModelRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

const modelColumns = `m.id, m.provider_id, p.name, m.name, m.aliases, m.display_name,
	m.version, m.context_window, m.max_output_tokens, m.capabilities,
	m.input_cost_per_million, m.output_cost_per_million, m.cached_input_cost_per_million,
	m.status, m.quality_tier, m.rate_limit_rpm, m.rate_limit_tpm, m.deprecated_at,
	m.environment, m.priority, m.managed_by, m.metadata, m.created_at, m.updated_at`

// Upsert creates or updates a model identified by provider and name.
func (r *ModelRepository) Upsert(ctx context.Context, m *domain.Model) (*domain.Model, error) {
	if m.ID == "" {
		m.ID = domain.NewID()
	}
	metadata, err := json.Marshal(orEmptyMap(m.Metadata))
	if err != nil {
		return nil, wrapDBError("encode model metadata", err)
	}
	aliases := m.Aliases
	if aliases == nil {
		aliases = []string{}
	}
	modelStatus := string(m.Status)
	if modelStatus == "" {
		modelStatus = string(domain.ModelActive)
	}

	row := r.pool.QueryRow(ctx, `
		INSERT INTO models (id, provider_id, name, aliases, display_name, version,
			context_window, max_output_tokens, capabilities, input_cost_per_million,
			output_cost_per_million, cached_input_cost_per_million, status, quality_tier,
			rate_limit_rpm, rate_limit_tpm, environment, priority, managed_by, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (provider_id, name) DO UPDATE
			SET aliases = EXCLUDED.aliases,
			    display_name = EXCLUDED.display_name,
			    version = EXCLUDED.version,
			    context_window = EXCLUDED.context_window,
			    max_output_tokens = EXCLUDED.max_output_tokens,
			    capabilities = EXCLUDED.capabilities,
			    input_cost_per_million = EXCLUDED.input_cost_per_million,
			    output_cost_per_million = EXCLUDED.output_cost_per_million,
			    cached_input_cost_per_million = EXCLUDED.cached_input_cost_per_million,
			    status = EXCLUDED.status,
			    quality_tier = EXCLUDED.quality_tier,
			    rate_limit_rpm = EXCLUDED.rate_limit_rpm,
			    rate_limit_tpm = EXCLUDED.rate_limit_tpm,
			    environment = EXCLUDED.environment,
			    priority = EXCLUDED.priority,
			    managed_by = EXCLUDED.managed_by,
			    metadata = EXCLUDED.metadata,
			    updated_at = now()
		RETURNING id`,
		m.ID, m.ProviderID, m.Name, aliases, m.DisplayName, m.Version, m.ContextWindow,
		m.MaxOutputTokens, capabilityStrings(m.Capabilities), m.InputCostPerMillion,
		m.OutputCostPerMillion, m.CachedInputCostPerMillion, modelStatus, m.QualityTier,
		m.RateLimitRPM, m.RateLimitTPM, envOrDefault(string(m.Environment)),
		defaultInt(m.Priority, 100), managedByOrDefault(string(m.ManagedBy)), metadata)
	if err != nil {
		return nil, wrapDBError("upsert model "+m.Name, err)
	}
	var id string
	if err := row.Scan(&id); err != nil {
		return nil, wrapDBError("upsert model", err)
	}
	return r.GetByID(ctx, id)
}

// List returns every model with its provider name resolved.
func (r *ModelRepository) List(ctx context.Context) ([]domain.Model, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+modelColumns+`
		  FROM models m
		  JOIN providers p ON p.id = m.provider_id
		 ORDER BY p.name, m.name`)
	if err != nil {
		return nil, wrapDBError("list models", err)
	}
	defer rows.Close()

	var out []domain.Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, wrapDBError("scan model", err)
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list models", err)
	}
	return out, nil
}

// GetByID returns a model by identifier.
func (r *ModelRepository) GetByID(ctx context.Context, id string) (*domain.Model, error) {
	row := r.pool.QueryRow(ctx, `
		SELECT `+modelColumns+`
		  FROM models m
		  JOIN providers p ON p.id = m.provider_id
		 WHERE m.id = $1`, id)
	m, err := scanModel(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load model", err)
	}
	return m, nil
}

// ListByProvider returns one provider's models with names resolved.
func (r *ModelRepository) ListByProvider(ctx context.Context, providerID string) ([]domain.Model, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT `+modelColumns+`
		  FROM models m
		  JOIN providers p ON p.id = m.provider_id
		 WHERE m.provider_id = $1
		 ORDER BY m.priority, m.name`, providerID)
	if err != nil {
		return nil, wrapDBError("list provider models", err)
	}
	defer rows.Close()

	var out []domain.Model
	for rows.Next() {
		m, err := scanModel(rows)
		if err != nil {
			return nil, wrapDBError("scan model", err)
		}
		out = append(out, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list provider models", err)
	}
	return out, nil
}

// Delete removes a model.
func (r *ModelRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM models WHERE id = $1`, id)
	if err != nil {
		return wrapDBError("delete model", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "model not found")
	}
	return nil
}

func scanModel(row pgx.Row) (*domain.Model, error) {
	var (
		m            domain.Model
		aliases      []string
		capabilities []string
		status       string
		environment  string
		managedBy    string
		metadata     []byte
	)
	if err := row.Scan(&m.ID, &m.ProviderID, &m.ProviderName, &m.Name, &aliases,
		&m.DisplayName, &m.Version, &m.ContextWindow, &m.MaxOutputTokens, &capabilities,
		&m.InputCostPerMillion, &m.OutputCostPerMillion, &m.CachedInputCostPerMillion,
		&status, &m.QualityTier, &m.RateLimitRPM, &m.RateLimitTPM, &m.DeprecatedAt,
		&environment, &m.Priority, &managedBy, &metadata, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return nil, err
	}
	m.Aliases = aliases
	m.Capabilities = toCapabilities(capabilities)
	m.Status = domain.ModelStatus(status)
	m.Environment = domain.Environment(environment)
	m.ManagedBy = domain.ManagedBy(managedBy)
	if len(metadata) > 0 {
		_ = json.Unmarshal(metadata, &m.Metadata)
	}
	return &m, nil
}

// ---------------------------------------------------------------------------
// Routing policies
// ---------------------------------------------------------------------------

// PolicyRepository stores routing policies.
//
// It implements policy.Repository, so it is passed directly to the policy
// resolver without an adapter layer.
type PolicyRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// Upsert creates or updates a policy identified by scope and name.
//
// Tenant scoping uses two partial unique indexes (one for tenant policies, one
// for global policies), so a single ON CONFLICT target cannot match both. The
// upsert is therefore read-then-write: look up the existing row by scope and
// name, UPDATE it by id when present, INSERT otherwise. The extra round trip
// happens only at bootstrap and admin-write time, never on the request path.
func (r *PolicyRepository) Upsert(ctx context.Context, p *domain.RoutingPolicy) (*domain.RoutingPolicy, error) {
	if p.ID == "" {
		p.ID = domain.NewID()
	}
	p.Normalize()

	matchJSON, err := json.Marshal(p.Match)
	if err != nil {
		return nil, wrapDBError("encode policy match", err)
	}
	targetsJSON, err := json.Marshal(orEmptySliceTargets(p.Targets))
	if err != nil {
		return nil, wrapDBError("encode policy targets", err)
	}
	fallbackJSON, err := json.Marshal(p.Fallback)
	if err != nil {
		return nil, wrapDBError("encode policy fallback", err)
	}
	retryJSON, err := json.Marshal(p.Retry)
	if err != nil {
		return nil, wrapDBError("encode policy retry", err)
	}
	timeoutJSON, err := json.Marshal(p.Timeout)
	if err != nil {
		return nil, wrapDBError("encode policy timeout", err)
	}
	limitsJSON, err := json.Marshal(p.Limits)
	if err != nil {
		return nil, wrapDBError("encode policy limits", err)
	}

	var existingID string
	if p.TenantID == "" {
		_ = r.pool.QueryRow(ctx,
			`SELECT id::text FROM routing_policies WHERE tenant_id IS NULL AND name = $1`,
			p.Name).Scan(&existingID)
	} else {
		_ = r.pool.QueryRow(ctx,
			`SELECT id::text FROM routing_policies WHERE tenant_id = $1 AND name = $2`,
			p.TenantID, p.Name).Scan(&existingID)
	}

	if existingID != "" {
		_, err = r.pool.Exec(ctx, `
			UPDATE routing_policies
			   SET description = $2, priority = $3, enabled = $4, match = $5,
			       strategy = $6, targets = $7, fallback = $8, retry = $9,
			       timeout = $10, limits = $11, version = version + 1,
			       managed_by = $12, updated_at = now()
			 WHERE id = $1`,
			existingID, p.Description, p.Priority, p.Enabled, matchJSON,
			string(p.Strategy), targetsJSON, fallbackJSON, retryJSON, timeoutJSON,
			limitsJSON, managedByOrDefault(string(p.ManagedBy)))
		if err != nil {
			return nil, wrapDBError("update routing policy "+p.Name, err)
		}
		return r.GetPolicy(ctx, existingID)
	}

	row := r.pool.QueryRow(ctx, `
		INSERT INTO routing_policies (id, tenant_id, name, description, priority, enabled,
			match, strategy, targets, fallback, retry, timeout, limits, version, created_by, managed_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,1,$14,$15)
		RETURNING `+policyColumns,
		p.ID, nullableUUID(p.TenantID), p.Name, p.Description, p.Priority, p.Enabled,
		matchJSON, string(p.Strategy), targetsJSON, fallbackJSON, retryJSON,
		timeoutJSON, limitsJSON, p.CreatedBy, managedByOrDefault(string(p.ManagedBy)))

	out, err := scanPolicy(row)
	if err != nil {
		return nil, wrapDBError("upsert routing policy "+p.Name, err)
	}
	return out, nil
}

// ListPolicies implements policy.Repository.
func (r *PolicyRepository) ListPolicies(ctx context.Context) ([]domain.RoutingPolicy, error) {
	rows, err := r.pool.Query(ctx, `SELECT `+policyColumns+` FROM routing_policies ORDER BY priority, name`)
	if err != nil {
		return nil, wrapDBError("list routing policies", err)
	}
	defer rows.Close()
	return collectPolicies(rows)
}

// GetPolicy implements policy.Repository.
func (r *PolicyRepository) GetPolicy(ctx context.Context, id string) (*domain.RoutingPolicy, error) {
	row := r.pool.QueryRow(ctx, `SELECT `+policyColumns+` FROM routing_policies WHERE id = $1`, id)
	p, err := scanPolicy(row)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, wrapDBError("load routing policy", err)
	}
	return p, nil
}

// Delete removes a policy.
func (r *PolicyRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM routing_policies WHERE id = $1`, id)
	if err != nil {
		return wrapDBError("delete routing policy", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "routing policy not found")
	}
	return nil
}

const policyColumns = `id, COALESCE(tenant_id::text, ''), name, description, priority, enabled,
	match, strategy, targets, fallback, retry, timeout, limits, version, created_by,
	managed_by, created_at, updated_at`

func scanPolicy(row pgx.Row) (*domain.RoutingPolicy, error) {
	var (
		p        domain.RoutingPolicy
		match    []byte
		strategy string
		targets  []byte
		fallback []byte
		retry    []byte
		timeout  []byte
		limits   []byte
		managed  string
	)
	if err := row.Scan(&p.ID, &p.TenantID, &p.Name, &p.Description, &p.Priority, &p.Enabled,
		&match, &strategy, &targets, &fallback, &retry, &timeout, &limits, &p.Version,
		&p.CreatedBy, &managed, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Strategy = domain.RoutingStrategy(strategy)
	p.ManagedBy = domain.ManagedBy(managed)

	// A decode failure on a nested block leaves the zero value in place, which
	// Normalize then fills with defaults. Refusing to load the policy would take
	// all traffic down for a cosmetic data problem.
	if len(match) > 0 {
		if err := json.Unmarshal(match, &p.Match); err != nil {
			return nil, wrapDBError("decode policy match", err)
		}
	}
	if len(targets) > 0 {
		if err := json.Unmarshal(targets, &p.Targets); err != nil {
			return nil, wrapDBError("decode policy targets", err)
		}
	}
	if len(fallback) > 0 {
		_ = json.Unmarshal(fallback, &p.Fallback)
	}
	if len(retry) > 0 {
		_ = json.Unmarshal(retry, &p.Retry)
	}
	if len(timeout) > 0 {
		_ = json.Unmarshal(timeout, &p.Timeout)
	}
	if len(limits) > 0 {
		_ = json.Unmarshal(limits, &p.Limits)
	}
	p.Normalize()
	return &p, nil
}

func collectPolicies(rows pgx.Rows) ([]domain.RoutingPolicy, error) {
	var out []domain.RoutingPolicy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, wrapDBError("scan routing policy", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list routing policies", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Settings
// ---------------------------------------------------------------------------

// SettingsRepository stores platform settings as JSON values.
type SettingsRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// Get returns a setting's raw JSON value.
func (r *SettingsRepository) Get(ctx context.Context, key string) (json.RawMessage, bool, error) {
	var raw []byte
	err := r.pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, key).Scan(&raw)
	if isNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, wrapDBError("load setting", err)
	}
	return json.RawMessage(raw), true, nil
}

// Set stores a setting.
func (r *SettingsRepository) Set(ctx context.Context, key string, value any, updatedBy string) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return wrapDBError("encode setting", err)
	}
	_, err = r.pool.Exec(ctx, `
		INSERT INTO settings (key, value, updated_by, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (key) DO UPDATE
			SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		key, raw, updatedBy)
	if err != nil {
		return wrapDBError("write setting", err)
	}
	return nil
}

// List returns every setting.
func (r *SettingsRepository) List(ctx context.Context) (map[string]json.RawMessage, error) {
	rows, err := r.pool.Query(ctx, `SELECT key, value FROM settings ORDER BY key`)
	if err != nil {
		return nil, wrapDBError("list settings", err)
	}
	defer rows.Close()

	out := map[string]json.RawMessage{}
	for rows.Next() {
		var (
			key string
			raw []byte
		)
		if err := rows.Scan(&key, &raw); err != nil {
			return nil, wrapDBError("scan setting", err)
		}
		out[key] = json.RawMessage(raw)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list settings", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// catalogueAdapter presents the provider and model repositories as a routing
// catalogue, which is how the engine reads the registry without importing storage.
type catalogueAdapter struct {
	providers *ProviderRepository
	models    *ModelRepository
}

// NewCatalogue adapts the repositories to the routing catalogue interface.
func NewCatalogue(providers *ProviderRepository, models *ModelRepository) *catalogueAdapter {
	return &catalogueAdapter{providers: providers, models: models}
}

// Models implements routing.Catalogue.
func (c *catalogueAdapter) Models(ctx context.Context) ([]domain.Model, error) {
	return c.models.List(ctx)
}

// Providers implements routing.Catalogue.
func (c *catalogueAdapter) Providers(ctx context.Context) ([]domain.Provider, error) {
	return c.providers.List(ctx)
}

func defaultID(id string) any {
	if id == "" {
		return domain.NewID()
	}
	return id
}

func statusOrDefault(status string) string {
	if status == "" {
		return string(domain.StatusActive)
	}
	return status
}

// managedByOrDefault stamps the catalogue owner, defaulting to the
// bootstrapper for rows that predate Phase 3 management.
func managedByOrDefault(managed string) string {
	if managed == "" {
		return string(domain.ManagedByBootstrap)
	}
	return managed
}

// envOrDefault defaults an empty environment to production, the only safe
// assumption for a row that predates environment metadata.
func envOrDefault(env string) string {
	if env == "" {
		return string(domain.EnvProduction)
	}
	return env
}

// authStyleOrDefault resolves the credential placement stored for a provider.
//
// It defers to the kind when the operator did not choose a style. Defaulting
// everything to bearer would silently break Anthropic, whose Messages API rejects
// bearer tokens: the provider would authenticate correctly in the adapter's
// kind-aware default and incorrectly once a concrete style was persisted.
func authStyleOrDefault(kind domain.ProviderKind, style string) string {
	if style == "" {
		return string(domain.DefaultAuthStyle(kind))
	}
	return style
}

// authHeaderOrDefault resolves the header name for header-authenticated
// providers, so a provider stored without an explicit name still sends its
// credential rather than silently omitting it.
func authHeaderOrDefault(kind domain.ProviderKind, header string) string {
	if header != "" {
		return header
	}
	return domain.DefaultAuthHeader(kind)
}

func defaultInt(value, fallback int) int {
	if value == 0 {
		return fallback
	}
	return value
}

// nullableUUID renders an empty (or non-UUID) identifier as SQL NULL rather
// than as an invalid UUID literal, which Postgres would reject.
//
// Non-UUID sentinels reach here from synthetic principals: the bootstrap admin
// key authenticates as tenant "system", which is a label, not a row id. Storing
// NULL keeps the audit/usage row (the actor remains identified by ActorLabel
// and the access logs) instead of losing the record to a type error.
func nullableUUID(id string) any {
	if id == "" {
		return nil
	}
	if _, err := uuid.Parse(id); err != nil {
		return nil
	}
	return id
}

func orEmptyMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func orEmptySliceTargets(t []domain.RouteTarget) []domain.RouteTarget {
	if t == nil {
		return []domain.RouteTarget{}
	}
	return t
}

func capabilityStrings(caps []domain.Capability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if c != "" {
			out = append(out, string(c))
		}
	}
	return out
}

func toCapabilities(values []string) []domain.Capability {
	out := make([]domain.Capability, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, domain.Capability(v))
		}
	}
	return out
}

// timePtr returns a pointer to a time value, for optional columns.
func timePtr(t time.Time) *time.Time { return &t }
