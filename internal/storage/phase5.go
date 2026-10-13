package storage

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Phase 5 repositories: cache policies, invalidation audit and entry
// metadata. Bodies live in Redis; these rows exist so the dashboard can
// explain cache behaviour and so every flush is auditable.

// ---------------------------------------------------------------------------
// Cache policies
// ---------------------------------------------------------------------------

// CachePolicyRepository stores per-scope caching rules.
type CachePolicyRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewCachePolicyRepository constructs the store.
func NewCachePolicyRepository(pool *pgxpool.Pool, logger *slog.Logger) *CachePolicyRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &CachePolicyRepository{pool: pool, logger: logger}
}

// Upsert creates or updates a policy by id.
func (r *CachePolicyRepository) Upsert(ctx context.Context, p *domain.CachePolicy) (*domain.CachePolicy, error) {
	if p.ID == "" {
		p.ID = domain.NewID()
	}
	if p.TTLSeconds <= 0 {
		p.TTLSeconds = 300
	}
	if p.Threshold <= 0 {
		p.Threshold = 0.92
	}
	semantic := false
	if p.Semantic != nil {
		semantic = *p.Semantic
	}
	prefix := false
	if p.Prefix != nil {
		prefix = *p.Prefix
	}
	bypassTools := true
	if p.BypassTools != nil {
		bypassTools = *p.BypassTools
	}
	allowNonDet := false
	if p.AllowNonDet != nil {
		allowNonDet = *p.AllowNonDet
	}
	row := r.pool.QueryRow(ctx, `
		INSERT INTO cache_policies (id, tenant_id, endpoint_id, api_key_id, provider, model,
			name, enabled, ttl_seconds, semantic_enabled, semantic_threshold,
			prefix_enabled, bypass_tools, allow_nondeterministic)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (id) DO UPDATE
			SET tenant_id = EXCLUDED.tenant_id,
			    endpoint_id = EXCLUDED.endpoint_id,
			    api_key_id = EXCLUDED.api_key_id,
			    provider = EXCLUDED.provider,
			    model = EXCLUDED.model,
			    name = EXCLUDED.name,
			    enabled = EXCLUDED.enabled,
			    ttl_seconds = EXCLUDED.ttl_seconds,
			    semantic_enabled = EXCLUDED.semantic_enabled,
			    semantic_threshold = EXCLUDED.semantic_threshold,
			    prefix_enabled = EXCLUDED.prefix_enabled,
			    bypass_tools = EXCLUDED.bypass_tools,
			    allow_nondeterministic = EXCLUDED.allow_nondeterministic,
			    updated_at = now()
		RETURNING id, COALESCE(tenant_id::text,''), endpoint_id, COALESCE(api_key_id::text,''),
			provider, model, name, enabled, ttl_seconds, semantic_enabled,
			semantic_threshold, prefix_enabled, bypass_tools, allow_nondeterministic,
			created_at, updated_at`,
		p.ID, nullableUUID(p.TenantID), p.EndpointID, nullableUUID(p.APIKeyID),
		p.Provider, p.Model, p.Name, p.Enabled, p.TTLSeconds, semantic,
		p.Threshold, prefix, bypassTools, allowNonDet)
	out, err := scanCachePolicy(row)
	if err != nil {
		return nil, wrapDBError("upsert cache policy", err)
	}
	return out, nil
}

// List returns policies, optionally scoped to a tenant.
func (r *CachePolicyRepository) List(ctx context.Context, tenantID string) ([]domain.CachePolicy, error) {
	var (
		rows interface {
			Close()
		}
		_ = rows
	)
	if tenantID == "" {
		rs, err := r.pool.Query(ctx, `
			SELECT id, COALESCE(tenant_id::text,''), endpoint_id, COALESCE(api_key_id::text,''),
				provider, model, name, enabled, ttl_seconds, semantic_enabled,
				semantic_threshold, prefix_enabled, bypass_tools, allow_nondeterministic,
				created_at, updated_at
			FROM cache_policies ORDER BY created_at DESC LIMIT 200`)
		if err != nil {
			return nil, wrapDBError("list cache policies", err)
		}
		defer rs.Close()
		return collectCachePolicies(rs)
	}
	rs, err := r.pool.Query(ctx, `
		SELECT id, COALESCE(tenant_id::text,''), endpoint_id, COALESCE(api_key_id::text,''),
			provider, model, name, enabled, ttl_seconds, semantic_enabled,
			semantic_threshold, prefix_enabled, bypass_tools, allow_nondeterministic,
			created_at, updated_at
		FROM cache_policies WHERE tenant_id::text = $1 OR tenant_id IS NULL
		ORDER BY created_at DESC LIMIT 200`, tenantID)
	if err != nil {
		return nil, wrapDBError("list cache policies", err)
	}
	defer rs.Close()
	return collectCachePolicies(rs)
}

// Delete removes a policy.
func (r *CachePolicyRepository) Delete(ctx context.Context, id string) error {
	tag, err := r.pool.Exec(ctx, `DELETE FROM cache_policies WHERE id = $1`, id)
	if err != nil {
		return wrapDBError("delete cache policy", err)
	}
	if tag.RowsAffected() == 0 {
		return domain.NewError(domain.ErrCodeNotFound, "cache policy not found")
	}
	return nil
}

func scanCachePolicy(row interface {
	Scan(dest ...any) error
}) (*domain.CachePolicy, error) {
	var (
		p           domain.CachePolicy
		semantic    bool
		prefix      bool
		bypassTools bool
		allowNonDet bool
	)
	if err := row.Scan(&p.ID, &p.TenantID, &p.EndpointID, &p.APIKeyID,
		&p.Provider, &p.Model, &p.Name, &p.Enabled, &p.TTLSeconds, &semantic,
		&p.Threshold, &prefix, &bypassTools, &allowNonDet,
		&p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.Semantic = &semantic
	p.Prefix = &prefix
	p.BypassTools = &bypassTools
	p.AllowNonDet = &allowNonDet
	return &p, nil
}

func collectCachePolicies(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close()
}) ([]domain.CachePolicy, error) {
	var out []domain.CachePolicy
	for rows.Next() {
		p, err := scanCachePolicy(rows)
		if err != nil {
			return nil, wrapDBError("scan cache policy", err)
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list cache policies", err)
	}
	return out, nil
}

// MatchForRequest resolves the most specific policy row for a request:
// key > endpoint > tenant+model > tenant+provider > tenant > global.
// Returns nil when no row matches, letting global config apply.
func (r *CachePolicyRepository) MatchForRequest(ctx context.Context, tenantID, endpointID, apiKeyID, provider, model string) (*domain.CachePolicy, error) {
	policies, err := r.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	var (
		key, endpoint, tenantModel, tenantProvider, tenant, global *domain.CachePolicy
	)
	for i := range policies {
		p := &policies[i]
		switch {
		case apiKeyID != "" && p.APIKeyID == apiKeyID:
			if key == nil {
				cp := *p
				key = &cp
			}
		case endpointID != "" && p.EndpointID == endpointID:
			if endpoint == nil {
				cp := *p
				endpoint = &cp
			}
		case p.TenantID == tenantID && p.Model != "" && (p.Model == model || p.Model == "*"):
			if tenantModel == nil {
				cp := *p
				tenantModel = &cp
			}
		case p.TenantID == tenantID && p.Provider != "" && (p.Provider == provider || p.Provider == "*"):
			if tenantProvider == nil {
				cp := *p
				tenantProvider = &cp
			}
		case p.TenantID == tenantID && p.Provider == "" && p.Model == "" && p.EndpointID == "" && p.APIKeyID == "":
			if tenant == nil {
				cp := *p
				tenant = &cp
			}
		case p.TenantID == "" && p.Provider == "" && p.Model == "" && p.EndpointID == "" && p.APIKeyID == "":
			if global == nil {
				cp := *p
				global = &cp
			}
		}
	}
	for _, c := range []*domain.CachePolicy{key, endpoint, tenantModel, tenantProvider, tenant, global} {
		if c != nil {
			return c, nil
		}
	}
	return nil, nil
}

// ---------------------------------------------------------------------------
// Cache invalidations
// ---------------------------------------------------------------------------

// CacheInvalidationRepository stores the flush audit trail.
type CacheInvalidationRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewCacheInvalidationRepository constructs the store.
func NewCacheInvalidationRepository(pool *pgxpool.Pool, logger *slog.Logger) *CacheInvalidationRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &CacheInvalidationRepository{pool: pool, logger: logger}
}

// Record inserts an invalidation event. Best-effort from the hot path: the
// caller logs but never fails the flush when this errors.
func (r *CacheInvalidationRepository) Record(ctx context.Context, e *domain.CacheInvalidationEvent) error {
	if e.ID == "" {
		e.ID = domain.NewID()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = domain.Now()
	}
	_, err := r.pool.Exec(ctx, `
		INSERT INTO cache_invalidations (id, tenant_id, scope, target, reason, actor, removed, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		e.ID, nullableUUID(e.TenantID), e.Scope, e.Target, e.Reason, e.Actor, e.Removed, e.CreatedAt)
	if err != nil {
		return wrapDBError("record cache invalidation", err)
	}
	return nil
}

// ListRecent returns the latest invalidation events.
func (r *CacheInvalidationRepository) ListRecent(ctx context.Context, tenantID string, limit int) ([]domain.CacheInvalidationEvent, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var out []domain.CacheInvalidationEvent
	if tenantID == "" {
		rows, err := r.pool.Query(ctx, `
			SELECT id, COALESCE(tenant_id::text,''), scope, target, reason, actor, removed, created_at
			FROM cache_invalidations ORDER BY created_at DESC LIMIT $1`, limit)
		if err != nil {
			return nil, wrapDBError("list cache invalidations", err)
		}
		defer rows.Close()
		for rows.Next() {
			var e domain.CacheInvalidationEvent
			if err := rows.Scan(&e.ID, &e.TenantID, &e.Scope, &e.Target, &e.Reason, &e.Actor, &e.Removed, &e.CreatedAt); err != nil {
				return nil, wrapDBError("scan cache invalidation", err)
			}
			out = append(out, e)
		}
		return out, rows.Err()
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, COALESCE(tenant_id::text,''), scope, target, reason, actor, removed, created_at
		FROM cache_invalidations WHERE tenant_id::text = $1 OR tenant_id IS NULL
		ORDER BY created_at DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, wrapDBError("list cache invalidations", err)
	}
	defer rows.Close()
	for rows.Next() {
		var e domain.CacheInvalidationEvent
		if err := rows.Scan(&e.ID, &e.TenantID, &e.Scope, &e.Target, &e.Reason, &e.Actor, &e.Removed, &e.CreatedAt); err != nil {
			return nil, wrapDBError("scan cache invalidation", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// Cache entries (metadata for inspect / top prompts)
// ---------------------------------------------------------------------------

// CacheEntryRepository stores lightweight entry metadata.
type CacheEntryRepository struct {
	pool   *pgxpool.Pool
	logger *slog.Logger
}

// NewCacheEntryRepository constructs the store.
func NewCacheEntryRepository(pool *pgxpool.Pool, logger *slog.Logger) *CacheEntryRepository {
	if logger == nil {
		logger = slog.Default()
	}
	return &CacheEntryRepository{pool: pool, logger: logger}
}

// UpsertMeta records or refreshes entry metadata after a store. The body
// stays in Redis; this row powers inspect and top-prompts.
func (r *CacheEntryRepository) UpsertMeta(ctx context.Context, e *domain.CacheEntry) error {
	if e.ID == "" {
		e.ID = domain.NewID()
	}
	now := domain.Now()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = now
	}
	e.UpdatedAt = now
	_, err := r.pool.Exec(ctx, `
		INSERT INTO cache_entries (id, tenant_id, kind, cache_key, model, prompt_hash,
			embedding_hash, similarity, sensitive, hit_count, expires_at, created_at, updated_at,
			provider, policy_id, endpoint_id, api_key_id, prompt_preview, reuse_count)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT (cache_key) DO UPDATE
			SET hit_count = cache_entries.hit_count + 1,
			    reuse_count = cache_entries.reuse_count + 1,
			    similarity = EXCLUDED.similarity,
			    expires_at = EXCLUDED.expires_at,
			    updated_at = now(),
			    prompt_preview = CASE WHEN EXCLUDED.prompt_preview <> '' THEN EXCLUDED.prompt_preview ELSE cache_entries.prompt_preview END`,
		e.ID, nullableUUID(e.TenantID), string(e.Kind), e.CacheKey, e.Model, e.PromptHash,
		e.EmbeddingHash, e.Similarity, e.Sensitive, e.HitCount, nullTime(e.ExpiresAt),
		e.CreatedAt, e.UpdatedAt, e.Provider, e.PolicyID, e.EndpointID,
		nullableUUID(e.APIKeyID), e.PromptPreview, e.ReuseCount)
	if err != nil {
		return wrapDBError("upsert cache entry", err)
	}
	return nil
}

// BumpHit increments hit/reuse counters after a hit.
func (r *CacheEntryRepository) BumpHit(ctx context.Context, cacheKey string) {
	_, _ = r.pool.Exec(ctx, `
		UPDATE cache_entries SET hit_count = hit_count + 1, reuse_count = reuse_count + 1,
			updated_at = now() WHERE cache_key = $1`, cacheKey)
}

// TopPrompts returns the most-reused entries for the dashboard.
func (r *CacheEntryRepository) TopPrompts(ctx context.Context, tenantID string, limit int) ([]domain.CacheEntry, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	var out []domain.CacheEntry
	if tenantID == "" {
		rows, err := r.pool.Query(ctx, `
			SELECT id, COALESCE(tenant_id::text,''), kind, cache_key, model, prompt_hash,
				COALESCE(embedding_hash,''), COALESCE(similarity,0), COALESCE(sensitive,false),
				COALESCE(hit_count,0), expires_at, created_at, updated_at,
				COALESCE(provider,''), COALESCE(policy_id,''), COALESCE(endpoint_id,''),
				COALESCE(api_key_id::text,''), COALESCE(prompt_preview,''), COALESCE(reuse_count,0)
			FROM cache_entries ORDER BY reuse_count DESC, hit_count DESC LIMIT $1`, limit)
		if err != nil {
			return nil, wrapDBError("list top cache entries", err)
		}
		defer rows.Close()
		return collectCacheEntries(rows)
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, COALESCE(tenant_id::text,''), kind, cache_key, model, prompt_hash,
			COALESCE(embedding_hash,''), COALESCE(similarity,0), COALESCE(sensitive,false),
			COALESCE(hit_count,0), expires_at, created_at, updated_at,
			COALESCE(provider,''), COALESCE(policy_id,''), COALESCE(endpoint_id,''),
			COALESCE(api_key_id::text,''), COALESCE(prompt_preview,''), COALESCE(reuse_count,0)
		FROM cache_entries WHERE tenant_id::text = $1
		ORDER BY reuse_count DESC, hit_count DESC LIMIT $2`, tenantID, limit)
	if err != nil {
		return nil, wrapDBError("list top cache entries", err)
	}
	defer rows.Close()
	_ = out
	return collectCacheEntries(rows)
}

// Inspect returns recent entries for the admin inspect view.
func (r *CacheEntryRepository) Inspect(ctx context.Context, tenantID, model, provider string, limit int) ([]domain.CacheEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	q := `
		SELECT id, COALESCE(tenant_id::text,''), kind, cache_key, model, prompt_hash,
			COALESCE(embedding_hash,''), COALESCE(similarity,0), COALESCE(sensitive,false),
			COALESCE(hit_count,0), expires_at, created_at, updated_at,
			COALESCE(provider,''), COALESCE(policy_id,''), COALESCE(endpoint_id,''),
			COALESCE(api_key_id::text,''), COALESCE(prompt_preview,''), COALESCE(reuse_count,0)
		FROM cache_entries WHERE ($1 = '' OR tenant_id::text = $1)
		  AND ($2 = '' OR model = $2) AND ($3 = '' OR provider = $3)
		ORDER BY updated_at DESC LIMIT $4`
	rows, err := r.pool.Query(ctx, q, tenantID, model, provider, limit)
	if err != nil {
		return nil, wrapDBError("inspect cache entries", err)
	}
	defer rows.Close()
	return collectCacheEntries(rows)
}

// DeleteScope removes metadata rows for a flush scope.
func (r *CacheEntryRepository) DeleteScope(ctx context.Context, tenantID, model, provider, key string) (int, error) {
	if key != "" {
		tag, err := r.pool.Exec(ctx, `DELETE FROM cache_entries WHERE cache_key = $1`, key)
		if err != nil {
			return 0, wrapDBError("delete cache entry", err)
		}
		return int(tag.RowsAffected()), nil
	}
	if tenantID == "" && model == "" && provider == "" {
		tag, err := r.pool.Exec(ctx, `DELETE FROM cache_entries`)
		if err != nil {
			return 0, wrapDBError("delete cache entries", err)
		}
		return int(tag.RowsAffected()), nil
	}
	tag, err := r.pool.Exec(ctx, `
		DELETE FROM cache_entries WHERE ($1 = '' OR tenant_id::text = $1)
		  AND ($2 = '' OR model = $2) AND ($3 = '' OR provider = $3)`,
		tenantID, model, provider)
	if err != nil {
		return 0, wrapDBError("delete cache entries", err)
	}
	return int(tag.RowsAffected()), nil
}

func collectCacheEntries(rows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}) ([]domain.CacheEntry, error) {
	var out []domain.CacheEntry
	for rows.Next() {
		var (
			e                    domain.CacheEntry
			kind                 string
			expiresAt            *time.Time
			createdAt, updatedAt time.Time
		)
		if err := rows.Scan(&e.ID, &e.TenantID, &kind, &e.CacheKey, &e.Model,
			&e.PromptHash, &e.EmbeddingHash, &e.Similarity, &e.Sensitive,
			&e.HitCount, &expiresAt, &createdAt, &updatedAt,
			&e.Provider, &e.PolicyID, &e.EndpointID, &e.APIKeyID,
			&e.PromptPreview, &e.ReuseCount); err != nil {
			return nil, wrapDBError("scan cache entry", err)
		}
		e.Kind = domain.CacheKind(kind)
		if expiresAt != nil {
			e.ExpiresAt = *expiresAt
		}
		e.CreatedAt = createdAt
		e.UpdatedAt = updatedAt
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, wrapDBError("list cache entries", err)
	}
	return out, nil
}

// nullTime renders a zero time as SQL NULL.
func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}
