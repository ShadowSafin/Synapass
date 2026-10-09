package api

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"

	adminsvc "github.com/shadowsafin/synapass/internal/admin"
	"github.com/shadowsafin/synapass/internal/auth"
	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/domain"
)

// This file implements the Phase 3 management surface: full CRUD for
// providers, models, tenants and API keys, encrypted credential storage,
// provider connectivity tests, and health-override management.
//
// Handler discipline follows the rest of the package: decode, delegate to the
// repository or admin service, audit, trigger a runtime reload, encode.
// Secrets are never written to logs or responses.

// ---------------------------------------------------------------------------
// Runtime reload
// ---------------------------------------------------------------------------

// Reloader refreshes in-memory runtime state after a catalogue write.
//
// The gateway already refreshes on an interval; the reload hook only makes an
// operator's save immediate. A failed reload is logged, never returned,
// because the interval loop will converge anyway.
type Reloader interface {
	ReloadCatalogue(ctx context.Context) error
}

// ReloaderFunc adapts a function to the Reloader interface.
type ReloaderFunc func(ctx context.Context) error

// ReloadCatalogue implements Reloader.
func (f ReloaderFunc) ReloadCatalogue(ctx context.Context) error { return f(ctx) }

// reloadRuntime applies a catalogue write immediately, best-effort.
func (s *Server) reloadRuntime(ctx context.Context) {
	if s.reloader == nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := s.reloader.ReloadCatalogue(cctx); err != nil {
		s.logger.Warn("runtime reload after an admin write failed; the background refresh will retry",
			"error", err)
	}
}

// credentialStore builds the sealing store from the environment.
//
// Key resolution is explicit SYNAPASS_CREDENTIALS_KEY first, admin-key derivation
// second, so stock deployments work with zero new configuration while serious
// deployments can pin an independent data key.
func (s *Server) credentialStore() (*adminsvc.Store, error) {
	var adminKey string
	if s.config != nil {
		adminKey = s.config.Auth.AdminKey
		if adminKey == "" && s.config.Auth.AdminKeyEnv != "" {
			adminKey = strings.TrimSpace(os.Getenv(s.config.Auth.AdminKeyEnv))
		}
	}
	key, err := adminsvc.KeyMaterial(
		strings.TrimSpace(os.Getenv(adminsvc.CredentialsKeyEnv)), adminKey)
	if err != nil {
		return nil, err
	}
	return adminsvc.NewStore(key)
}

func actorLabel(ctx context.Context) string {
	if p := principal(ctx); p != nil {
		return p.Label()
	}
	return ""
}

// ---------------------------------------------------------------------------
// Providers
// ---------------------------------------------------------------------------

// providerDetail is the single-provider view: full configuration plus
// credential presence. The embedded domain.Provider never serializes a secret
// (APIKeyInline is json:"-"), so this type is safe to encode directly.
type providerDetail struct {
	domain.Provider
	HasCredential bool                   `json:"has_credential"`
	Credential    *domain.CredentialMeta `json:"credential,omitempty"`
}

func (s *Server) providerDetail(ctx context.Context, p *domain.Provider) (providerDetail, error) {
	out := providerDetail{Provider: *p}
	if s.repos == nil || s.repos.Credentials == nil {
		return out, nil
	}
	meta, err := s.repos.Credentials.Meta(ctx, p.ID)
	if err != nil {
		return out, err
	}
	if meta != nil {
		out.HasCredential = true
		out.Credential = meta
	}
	return out, nil
}

func providerAfter(p *domain.Provider) map[string]any {
	return map[string]any{
		"name":        p.Name,
		"kind":        string(p.Kind),
		"base_url":    p.BaseURL,
		"status":      string(p.Status),
		"priority":    p.Priority,
		"weight":      p.Weight,
		"environment": string(p.Environment),
		"managed_by":  string(p.ManagedBy),
	}
}

// handleAdminGetProvider serves GET /admin/v1/providers/{id}.
func (s *Server) handleAdminGetProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	p, err := s.repos.Providers.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if p == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}
	detail, err := s.providerDetail(ctx, p)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleAdminCreateProvider serves POST /admin/v1/providers.
func (s *Server) handleAdminCreateProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	var body domain.Provider
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.ID == "" {
		body.ID = domain.NewID()
	}
	body.ManagedBy = domain.ManagedByAPI
	// A creation must not smuggle a plaintext secret: secrets travel through
	// the credential endpoint, where they are sealed before storage.
	body.APIKeyInline = ""
	adminsvc.NormalizeProvider(&body)
	if err := adminsvc.ValidateProvider(&body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	if existing, err := s.repos.Providers.GetByName(ctx, body.Name); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	} else if existing != nil {
		writeError(w, domain.Errorf(domain.ErrCodeInvalidRequest,
			"provider %q already exists", body.Name), metaFromContext(rc, nil))
		return
	}

	saved, err := s.repos.Providers.Upsert(ctx, &body)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceProvider, saved.ID, nil, providerAfter(saved))
	s.reloadRuntime(ctx)

	detail, err := s.providerDetail(ctx, saved)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusCreated, detail)
}

// handleAdminUpdateProvider serves PUT /admin/v1/providers/{id}: full
// replacement. The name is immutable (it is the natural key); delete and
// recreate to rename a provider.
func (s *Server) handleAdminUpdateProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	existing, err := s.repos.Providers.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}

	var body domain.Provider
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.Name != "" && body.Name != existing.Name {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"provider name is immutable; delete and recreate to rename"),
			metaFromContext(rc, nil))
		return
	}
	before := providerAfter(existing)

	body.ID = existing.ID
	body.Name = existing.Name
	body.ManagedBy = domain.ManagedByAPI
	body.APIKeyInline = ""
	adminsvc.NormalizeProvider(&body)
	if err := adminsvc.ValidateProvider(&body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	saved, err := s.repos.Providers.Upsert(ctx, &body)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceProvider, saved.ID, before, providerAfter(saved))
	s.reloadRuntime(ctx)
	s.cacheFlushBestEffort(ctx, domain.CacheScopeProvider, "", saved.Name, domain.CacheInvalidateProviderChange, actorLabel(ctx))

	detail, err := s.providerDetail(ctx, saved)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// providerPatch is the partial-update body of PATCH /admin/v1/providers/{id}.
// Pointers distinguish "absent" from "explicitly zero".
type providerPatch struct {
	BaseURL        *string           `json:"base_url,omitempty"`
	APIKeyEnv      *string           `json:"api_key_env,omitempty"`
	AuthStyle      *string           `json:"auth_style,omitempty"`
	HeaderName     *string           `json:"header_name,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Organization   *string           `json:"organization,omitempty"`
	Project        *string           `json:"project,omitempty"`
	Capabilities   []string          `json:"capabilities,omitempty"`
	Status         *string           `json:"status,omitempty"`
	Weight         *int              `json:"weight,omitempty"`
	Priority       *int              `json:"priority,omitempty"`
	TimeoutMS      *int              `json:"timeout_ms,omitempty"`
	MaxConcurrency *int              `json:"max_concurrency,omitempty"`
	Region         *string           `json:"region,omitempty"`
	Notes          *string           `json:"notes,omitempty"`
	Environment    *string           `json:"environment,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

// handleAdminPatchProvider serves PATCH /admin/v1/providers/{id}.
func (s *Server) handleAdminPatchProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	existing, err := s.repos.Providers.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}

	var body providerPatch
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	before := providerAfter(existing)

	merged := *existing
	if body.BaseURL != nil {
		merged.BaseURL = *body.BaseURL
	}
	if body.APIKeyEnv != nil {
		merged.APIKeyEnv = *body.APIKeyEnv
	}
	if body.AuthStyle != nil {
		merged.AuthStyle = domain.AuthStyle(*body.AuthStyle)
	}
	if body.HeaderName != nil {
		merged.HeaderName = *body.HeaderName
	}
	if body.Headers != nil {
		merged.Headers = body.Headers
	}
	if body.Organization != nil {
		merged.Organization = *body.Organization
	}
	if body.Project != nil {
		merged.Project = *body.Project
	}
	if body.Capabilities != nil {
		merged.Capabilities = toDomainCapabilities(body.Capabilities)
	}
	if body.Status != nil {
		merged.Status = domain.Status(*body.Status)
	}
	if body.Weight != nil {
		merged.Weight = *body.Weight
	}
	if body.Priority != nil {
		merged.Priority = *body.Priority
	}
	if body.TimeoutMS != nil {
		merged.TimeoutMS = *body.TimeoutMS
	}
	if body.MaxConcurrency != nil {
		merged.MaxConcurrency = *body.MaxConcurrency
	}
	if body.Region != nil {
		merged.Region = *body.Region
	}
	if body.Notes != nil {
		merged.Notes = *body.Notes
	}
	if body.Environment != nil {
		merged.Environment = domain.Environment(*body.Environment)
	}
	if body.Labels != nil {
		merged.Labels = body.Labels
	}
	merged.ManagedBy = domain.ManagedByAPI
	merged.APIKeyInline = ""
	adminsvc.NormalizeProvider(&merged)
	if err := adminsvc.ValidateProvider(&merged); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	saved, err := s.repos.Providers.Upsert(ctx, &merged)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	action := domain.AuditUpdate
	if body.Status != nil {
		if merged.Status == domain.StatusDisabled {
			action = domain.AuditDisable
		} else if existing.Status == domain.StatusDisabled && merged.Status.IsUsable() {
			action = domain.AuditEnable
		}
	}
	s.audit(ctx, rc, action, domain.ResourceProvider, saved.ID, before, providerAfter(saved))
	s.reloadRuntime(ctx)
	s.cacheFlushBestEffort(ctx, domain.CacheScopeProvider, "", saved.Name, domain.CacheInvalidateProviderChange, actorLabel(ctx))

	detail, err := s.providerDetail(ctx, saved)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleAdminDeleteProvider serves DELETE /admin/v1/providers/{id}. Models
// cascade by foreign key; the audit event records how many went with it.
func (s *Server) handleAdminDeleteProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	existing, err := s.repos.Providers.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}

	modelCount := 0
	if s.repos.Models != nil {
		if models, merr := s.repos.Models.ListByProvider(ctx, id); merr == nil {
			modelCount = len(models)
		}
	}

	if err := s.repos.Providers.Delete(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditDelete, domain.ResourceProvider, id, providerAfter(existing), map[string]any{
		"models_removed": modelCount,
	})
	s.reloadRuntime(ctx)
	s.cacheFlushBestEffort(ctx, domain.CacheScopeProvider, "", existing.Name, domain.CacheInvalidateProviderChange, actorLabel(ctx))

	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "models_removed": modelCount})
}

// toDomainCapabilities converts raw strings, dropping blanks.
func toDomainCapabilities(raw []string) []domain.Capability {
	out := make([]domain.Capability, 0, len(raw))
	for _, c := range raw {
		if strings.TrimSpace(c) != "" {
			out = append(out, domain.Capability(c))
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Provider credentials
// ---------------------------------------------------------------------------

// credentialRequest is the body of PUT /admin/v1/providers/{id}/credential.
type credentialRequest struct {
	// Secret is the plaintext upstream credential. It is sealed immediately
	// and never logged, returned, or audited.
	Secret string `json:"secret"`
	// Name is an operator label for the audit view.
	Name string `json:"name,omitempty"`
	// SyncModels discovers the provider's remote models into the registry
	// right after the credential is stored, so a new provider is usable in
	// one call.
	SyncModels bool `json:"sync_models,omitempty"`
}

// handleAdminSetProviderCredential seals and stores a provider secret.
func (s *Server) handleAdminSetProviderCredential(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil || s.repos.Credentials == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the credential store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	provider, err := s.repos.Providers.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if provider == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}

	var body credentialRequest
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	store, err := s.credentialStore()
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	env, err := store.Seal(body.Secret)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	env.ProviderID = provider.ID
	env.Name = body.Name
	env.CreatedBy = actorLabel(ctx)

	rotating := false
	if current, gerr := s.repos.Credentials.Get(ctx, provider.ID); gerr != nil {
		writeError(w, gerr, metaFromContext(rc, nil))
		return
	} else if current != nil {
		rotating = true
	}

	meta, err := s.repos.Credentials.Set(ctx, env)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	// Mark the provider api-managed so the bootstrapper never reverts the
	// setup this credential belongs to.
	provider.ManagedBy = domain.ManagedByAPI
	if _, err := s.repos.Providers.Upsert(ctx, provider); err != nil {
		s.logger.Warn("failed to mark a provider api-managed after a credential write",
			"provider", provider.Name, "error", err)
	}

	action := domain.AuditCreate
	if rotating {
		action = domain.AuditRotate
	}
	s.audit(ctx, rc, action, domain.ResourceCredential, provider.ID, nil, map[string]any{
		"name":        meta.Name,
		"key_version": meta.KeyVersion,
		"provider":    provider.Name,
	})
	s.reloadRuntime(ctx)

	resp := map[string]any{
		"provider_id":    provider.ID,
		"has_credential": true,
		"credential":     meta,
	}
	if body.SyncModels && s.repos.Models != nil {
		adapter, aerr := s.resolveProviderAdapter(ctx, provider)
		if aerr != nil {
			resp["sync_error"] = aerr.Error()
		} else if res, serr := adminsvc.SyncModels(ctx, adapter, s.repos.Models, adminsvc.SyncOptions{
			ProviderID:   provider.ID,
			ProviderName: provider.Name,
			Environment:  provider.Environment,
			CreatedBy:    actorLabel(ctx),
			Timeout:      20 * time.Second,
		}); serr != nil {
			resp["sync_error"] = serr.Error()
		} else {
			s.audit(ctx, rc, domain.AuditCreate, domain.ResourceModel, provider.ID, nil, map[string]any{
				"action": "sync", "provider": provider.Name,
				"created": len(res.Created), "skipped": len(res.Skipped), "total": res.Total,
			})
			s.reloadRuntime(ctx)
			resp["sync"] = res
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// handleAdminGetProviderCredential serves GET /admin/v1/providers/{id}/credential.
// It returns metadata only; the secret itself is never retrievable.
func (s *Server) handleAdminGetProviderCredential(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Credentials == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the credential store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	meta, err := s.repos.Credentials.Meta(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if meta == nil {
		writeJSON(w, http.StatusOK, map[string]any{"has_credential": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"has_credential": true, "credential": meta})
}

// handleAdminDeleteProviderCredential serves DELETE /admin/v1/providers/{id}/credential.
func (s *Server) handleAdminDeleteProviderCredential(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Credentials == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the credential store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	if err := s.repos.Credentials.Delete(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditDelete, domain.ResourceCredential, id, nil, nil)
	s.reloadRuntime(ctx)

	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---------------------------------------------------------------------------
// Provider connectivity tests
// ---------------------------------------------------------------------------

// providerTestRequest is the body of POST /admin/v1/providers/{id}/test.
type providerTestRequest struct {
	Checks []string `json:"checks,omitempty"`
	Model  string   `json:"model,omitempty"`
	Prompt string   `json:"prompt,omitempty"`
}

// handleAdminTestProvider runs connectivity, listing and sample checks.
func (s *Server) handleAdminTestProvider(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 90*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil || s.repos.TestResults == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	if s.adapters == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "no providers are configured"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	provider, err := s.repos.Providers.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if provider == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}

	var body providerTestRequest
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	checks := make([]domain.TestCheck, 0, len(body.Checks))
	for _, c := range body.Checks {
		check := domain.TestCheck(c)
		if !check.Valid() {
			writeError(w, domain.Errorf(domain.ErrCodeInvalidRequest,
				"unknown check %q: expected connectivity, models or sample", c),
				metaFromContext(rc, nil))
			return
		}
		checks = append(checks, check)
	}

	// Reload first so a provider created or edited moments ago already has an
	// adapter; otherwise the test would report "not configured" for a row
	// that is plainly there.
	adapter, err := s.resolveProviderAdapter(ctx, provider)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	results := adminsvc.Run(ctx, adapter, s.repos.Models, adminsvc.TestOptions{
		Checks:       checks,
		Model:        body.Model,
		Prompt:       body.Prompt,
		Timeout:      15 * time.Second,
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		CreatedBy:    actorLabel(ctx),
	})

	stored := 0
	allOK := true
	for i := range results {
		results[i].CreatedBy = actorLabel(ctx)
		if err := s.repos.TestResults.Insert(ctx, &results[i]); err != nil {
			s.logger.Warn("failed to persist a provider test result",
				"provider", provider.Name, "error", err)
			continue
		}
		stored++
		if !results[i].Success {
			allOK = false
		}
	}

	s.audit(ctx, rc, domain.AuditTest, domain.ResourceProvider, provider.ID, nil, map[string]any{
		"success": allOK,
		"checks":  len(results),
		"stored":  stored,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"provider_id": provider.ID,
		"success":     allOK,
		"results":     results,
	})
}

// handleAdminListProviderTests serves GET /admin/v1/providers/{id}/tests.
func (s *Server) handleAdminListProviderTests(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.TestResults == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the test store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	results, err := s.repos.TestResults.ListRecent(ctx, chiURLParam(r, "id"), parseIntParam(r, "limit", 50))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// resolveProviderAdapter reloads the runtime and resolves a working adapter
// for the provider, so endpoints acting on a just-saved provider never report
// "not configured" for a row that is plainly there.
func (s *Server) resolveProviderAdapter(ctx context.Context, provider *domain.Provider) (adminsvc.TestAdapter, error) {
	s.reloadRuntime(ctx)
	if s.adapters == nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "no providers are configured")
	}
	adapter, ok := s.adapters.Resolve(provider.ID)
	if !ok {
		adapter, ok = s.adapters.Resolve(provider.Name)
	}
	if !ok {
		return nil, domain.Errorf(domain.ErrCodeUnavailable,
			"provider %q has no working adapter; check its base_url and credentials", provider.Name)
	}
	return adapter, nil
}

// handleAdminSyncProviderModels serves POST /admin/v1/providers/{id}/sync-models.
//
// Discovery populates the registry with every remote model name. Existing
// rows are never modified, so operator edits (pricing, aliases, disables)
// survive re-syncs; delete a row to drop a model permanently.
func (s *Server) handleAdminSyncProviderModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 60*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil || s.repos.Models == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	provider, err := s.repos.Providers.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if provider == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}

	adapter, err := s.resolveProviderAdapter(ctx, provider)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	res, err := adminsvc.SyncModels(ctx, adapter, s.repos.Models, adminsvc.SyncOptions{
		ProviderID:   provider.ID,
		ProviderName: provider.Name,
		Environment:  provider.Environment,
		CreatedBy:    actorLabel(ctx),
		Timeout:      20 * time.Second,
	})
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceModel, provider.ID, nil, map[string]any{
		"action":   "sync",
		"provider": provider.Name,
		"created":  len(res.Created),
		"skipped":  len(res.Skipped),
		"total":    res.Total,
	})
	s.reloadRuntime(ctx)

	// When automatic detection is switched on, models that declare no
	// capabilities are asked what they can do before the response goes out.
	// Arrivals and backlog alike, bounded by max_models_per_run: a provider
	// with sixty silent models fills ten per sync rather than billing 240
	// probe calls at once. This is opt-in because it spends upstream tokens;
	// an explicit call to the endpoint below needs no flag.
	var report *adminsvc.DetectionReport
	if s.config.Detection.Enabled {
		dopts := detectionOptionsFromConfig(s.config.Detection)
		if r, derr := adminsvc.DetectMissingCapabilities(ctx, adapter, s.repos.Models, *provider, dopts); derr == nil && r != nil {
			report = r
			s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceModel, provider.ID, nil, map[string]any{
				"action":        "detect-capabilities",
				"provider":      provider.Name,
				"attempted":     len(report.Attempted),
				"proven":        len(report.Proven),
				"indeterminate": len(report.Indeterminate),
			})
			s.reloadRuntime(ctx)
		}
		// Detection failures must not fail a sync that otherwise succeeded: the
		// registry is correct, it just learned nothing new. They are audit-logged
		// by the endpoint when run explicitly.
	}

	// Model capabilities change nothing while the provider row stays empty:
	// routing gates on the provider-wide list first. Union what the models
	// now declare into the provider when it declares nothing, so a sync that
	// learned tools actually routes tools. An operator's provider list is
	// never touched, and an empty union writes nothing.
	reconciled, rerr := adminsvc.ReconcileProviderCapabilities(ctx, s.repos.Providers, s.repos.Models, *provider)
	if rerr == nil && reconciled != nil && reconciled.Filled {
		s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceProvider, provider.ID, nil, map[string]any{
			"action":       "reconcile-capabilities",
			"provider":     provider.Name,
			"capabilities": reconciled.Capabilities,
			"counted":      reconciled.ModelsCounted,
		})
		s.reloadRuntime(ctx)
	}
	// Reconcile failures are non-fatal like detection failures: the model
	// registry is correct, only the provider row learned nothing new.

	if report != nil || (reconciled != nil && reconciled.Filled) {
		out := map[string]any{"sync": res}
		if report != nil {
			out["detection"] = report
		}
		if reconciled != nil && reconciled.Filled {
			out["provider_capabilities"] = reconciled
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	writeJSON(w, http.StatusOK, res)
}

// handleAdminDetectCapabilities asks a provider's models what they can do.
//
// Probing is explicit billable traffic — typically four minimal completions
// per model — so besides the automatic post-sync run this endpoint is the
// operator's deliberate way to say "spend the tokens". It never touches a
// model that already declares capabilities.
func (s *Server) handleAdminDetectCapabilities(w http.ResponseWriter, r *http.Request) {
	// Detection is slow on purpose: up to MaxModelsPerRun models, each with
	// several round trips. Ten minutes bounds the worst case; per-model
	// timeouts and context cancellation end it sooner.
	ctx, cancel := timeoutContext(r, 10*time.Minute)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Providers == nil || s.repos.Models == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	provider, err := s.repos.Providers.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if provider == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}

	adapter, err := s.resolveProviderAdapter(ctx, provider)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	var body struct {
		Models          []string `json:"models"`
		MaxModels       *int     `json:"max_models"`
		Concurrency     *int     `json:"concurrency"`
		TimeoutPerModel string   `json:"timeout_per_model"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	opts := detectionOptionsFromConfig(s.config.Detection)
	opts.Only = body.Models
	if body.MaxModels != nil {
		opts.MaxModels = *body.MaxModels
	}
	if body.Concurrency != nil {
		opts.Concurrency = *body.Concurrency
	}
	if body.TimeoutPerModel != "" {
		if d, perr := time.ParseDuration(body.TimeoutPerModel); perr == nil && d > 0 {
			opts.Timeout = d
		}
	}

	report, err := adminsvc.DetectMissingCapabilities(ctx, adapter, s.repos.Models, *provider, opts)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceModel, provider.ID, nil, map[string]any{
		"action":        "detect-capabilities",
		"provider":      provider.Name,
		"attempted":     len(report.Attempted),
		"proven":        len(report.Proven),
		"indeterminate": len(report.Indeterminate),
	})
	s.reloadRuntime(ctx)

	// Same union as the post-sync run: proven model capabilities change
	// nothing while the provider row stays empty, so fill it when it
	// declares nothing. An operator's provider list is never touched.
	if reconciled, rerr := adminsvc.ReconcileProviderCapabilities(ctx, s.repos.Providers, s.repos.Models, *provider); rerr == nil && reconciled != nil && reconciled.Filled {
		report.ProviderCapabilitiesFilled = reconciled.Capabilities
		s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceProvider, provider.ID, nil, map[string]any{
			"action":       "reconcile-capabilities",
			"provider":     provider.Name,
			"capabilities": reconciled.Capabilities,
			"counted":      reconciled.ModelsCounted,
		})
		s.reloadRuntime(ctx)
	}

	writeJSON(w, http.StatusOK, report)
}

// detectionOptionsFromConfig translates the configured detection bounds into
// runner options. The endpoint stays usable when the automatic run is off, so
// this is shared rather than gated.
func detectionOptionsFromConfig(c config.DetectionConfig) adminsvc.DetectionOptions {
	timeout := 90 * time.Second
	if c.TimeoutPerModel.Std() > 0 {
		timeout = c.TimeoutPerModel.Std()
	}
	return adminsvc.DetectionOptions{
		Timeout:     timeout,
		Concurrency: c.Concurrency,
		MaxModels:   c.MaxModelsPerRun,
	}
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

func modelAfter(m *domain.Model) map[string]any {
	return map[string]any{
		"name": m.Name, "provider_id": m.ProviderID, "status": string(m.Status),
		"priority": m.Priority, "environment": string(m.Environment),
		"managed_by": string(m.ManagedBy),
	}
}

// handleAdminGetModel serves GET /admin/v1/models/{id}.
func (s *Server) handleAdminGetModel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Models == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the model registry is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	m, err := s.repos.Models.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if m == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "model not found"),
			metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// handleAdminCreateModel serves POST /admin/v1/models.
func (s *Server) handleAdminCreateModel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Models == nil || s.repos.Providers == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the model registry is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	var body domain.Model
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.ID == "" {
		body.ID = domain.NewID()
	}
	body.ManagedBy = domain.ManagedByAPI
	adminsvc.NormalizeModel(&body)
	if err := adminsvc.ValidateModel(&body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	provider, err := s.repos.Providers.GetByID(ctx, body.ProviderID)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if provider == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "provider not found"),
			metaFromContext(rc, nil))
		return
	}
	if existing, err := s.repos.Models.ListByProvider(ctx, body.ProviderID); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	} else {
		for _, m := range existing {
			if m.Name == body.Name {
				writeError(w, domain.Errorf(domain.ErrCodeInvalidRequest,
					"model %q already exists on provider %q", body.Name, provider.Name),
					metaFromContext(rc, nil))
				return
			}
		}
	}

	saved, err := s.repos.Models.Upsert(ctx, &body)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceModel, saved.ID, nil, modelAfter(saved))
	s.reloadRuntime(ctx)
	s.cacheFlushBestEffort(ctx, domain.CacheScopeModel, "", saved.Name, domain.CacheInvalidateModelChange, actorLabel(ctx))

	writeJSON(w, http.StatusCreated, saved)
}

// handleAdminUpdateModel serves PUT /admin/v1/models/{id}: full replacement.
// The name and provider are immutable (they are the natural key); delete and
// recreate to move a model.
func (s *Server) handleAdminUpdateModel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Models == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the model registry is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	existing, err := s.repos.Models.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "model not found"),
			metaFromContext(rc, nil))
		return
	}

	var body domain.Model
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.Name != "" && body.Name != existing.Name {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"model name is immutable; delete and recreate to rename"),
			metaFromContext(rc, nil))
		return
	}
	if body.ProviderID != "" && body.ProviderID != existing.ProviderID {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"a model cannot move providers; delete and recreate under the new provider"),
			metaFromContext(rc, nil))
		return
	}
	before := modelAfter(existing)

	body.ID = existing.ID
	body.ProviderID = existing.ProviderID
	body.Name = existing.Name
	body.ManagedBy = domain.ManagedByAPI
	adminsvc.NormalizeModel(&body)
	if err := adminsvc.ValidateModel(&body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	saved, err := s.repos.Models.Upsert(ctx, &body)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceModel, saved.ID, before, modelAfter(saved))
	s.reloadRuntime(ctx)
	s.cacheFlushBestEffort(ctx, domain.CacheScopeModel, "", saved.Name, domain.CacheInvalidateModelChange, actorLabel(ctx))

	writeJSON(w, http.StatusOK, saved)
}

// modelPatch is the partial-update body of PATCH /admin/v1/models/{id}.
type modelPatch struct {
	Aliases                   []string          `json:"aliases,omitempty"`
	DisplayName               *string           `json:"display_name,omitempty"`
	Version                   *string           `json:"version,omitempty"`
	ContextWindow             *int              `json:"context_window,omitempty"`
	MaxOutputTokens           *int              `json:"max_output_tokens,omitempty"`
	Capabilities              []string          `json:"capabilities,omitempty"`
	InputCostPerMillion       *float64          `json:"input_cost_per_million,omitempty"`
	OutputCostPerMillion      *float64          `json:"output_cost_per_million,omitempty"`
	CachedInputCostPerMillion *float64          `json:"cached_input_cost_per_million,omitempty"`
	Status                    *string           `json:"status,omitempty"`
	QualityTier               *int              `json:"quality_tier,omitempty"`
	RateLimitRPM              *int              `json:"rate_limit_rpm,omitempty"`
	RateLimitTPM              *int              `json:"rate_limit_tpm,omitempty"`
	Priority                  *int              `json:"priority,omitempty"`
	Environment               *string           `json:"environment,omitempty"`
	Metadata                  map[string]string `json:"metadata,omitempty"`
}

// handleAdminPatchModel serves PATCH /admin/v1/models/{id}.
func (s *Server) handleAdminPatchModel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Models == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the model registry is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	existing, err := s.repos.Models.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "model not found"),
			metaFromContext(rc, nil))
		return
	}

	var body modelPatch
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	before := modelAfter(existing)

	merged := *existing
	if body.Aliases != nil {
		merged.Aliases = body.Aliases
	}
	if body.DisplayName != nil {
		merged.DisplayName = *body.DisplayName
	}
	if body.Version != nil {
		merged.Version = *body.Version
	}
	if body.ContextWindow != nil {
		merged.ContextWindow = *body.ContextWindow
	}
	if body.MaxOutputTokens != nil {
		merged.MaxOutputTokens = *body.MaxOutputTokens
	}
	if body.Capabilities != nil {
		merged.Capabilities = toDomainCapabilities(body.Capabilities)
	}
	if body.InputCostPerMillion != nil {
		merged.InputCostPerMillion = *body.InputCostPerMillion
	}
	if body.OutputCostPerMillion != nil {
		merged.OutputCostPerMillion = *body.OutputCostPerMillion
	}
	if body.CachedInputCostPerMillion != nil {
		merged.CachedInputCostPerMillion = *body.CachedInputCostPerMillion
	}
	if body.Status != nil {
		merged.Status = domain.ModelStatus(*body.Status)
	}
	if body.QualityTier != nil {
		merged.QualityTier = *body.QualityTier
	}
	if body.RateLimitRPM != nil {
		merged.RateLimitRPM = *body.RateLimitRPM
	}
	if body.RateLimitTPM != nil {
		merged.RateLimitTPM = *body.RateLimitTPM
	}
	if body.Priority != nil {
		merged.Priority = *body.Priority
	}
	if body.Environment != nil {
		merged.Environment = domain.Environment(*body.Environment)
	}
	if body.Metadata != nil {
		merged.Metadata = body.Metadata
	}
	merged.ManagedBy = domain.ManagedByAPI
	// Priority zero is unset, not "highest": keep the stored value when the
	// patch does not mention it.
	if body.Priority == nil && merged.Priority == 0 {
		merged.Priority = 100
	}
	if err := adminsvc.ValidateModel(&merged); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	saved, err := s.repos.Models.Upsert(ctx, &merged)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	action := domain.AuditUpdate
	if body.Status != nil {
		if merged.Status == domain.ModelDisabled {
			action = domain.AuditDisable
		} else if existing.Status == domain.ModelDisabled && merged.Status.Usable() {
			action = domain.AuditEnable
		}
	}
	s.audit(ctx, rc, action, domain.ResourceModel, saved.ID, before, modelAfter(saved))
	s.reloadRuntime(ctx)
	s.cacheFlushBestEffort(ctx, domain.CacheScopeModel, "", saved.Name, domain.CacheInvalidateModelChange, actorLabel(ctx))

	writeJSON(w, http.StatusOK, saved)
}

// handleAdminDeleteModel serves DELETE /admin/v1/models/{id}.
func (s *Server) handleAdminDeleteModel(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Models == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the model registry is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	existing, err := s.repos.Models.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "model not found"),
			metaFromContext(rc, nil))
		return
	}

	if err := s.repos.Models.Delete(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditDelete, domain.ResourceModel, id, modelAfter(existing), nil)
	s.reloadRuntime(ctx)
	s.cacheFlushBestEffort(ctx, domain.CacheScopeModel, "", existing.Name, domain.CacheInvalidateModelChange, actorLabel(ctx))

	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// ---------------------------------------------------------------------------
// Tenants
// ---------------------------------------------------------------------------

func tenantAfter(t *domain.Tenant) map[string]any {
	return map[string]any{
		"slug": t.Slug, "name": t.Name, "status": string(t.Status),
		"plan": t.Plan, "default_routing_policy_id": t.DefaultRoutingPolicyID,
	}
}

// handleAdminGetTenant serves GET /admin/v1/tenants/{id}.
func (s *Server) handleAdminGetTenant(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Tenants == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the tenant store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	t, err := s.repos.Tenants.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if t == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "tenant not found"),
			metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// tenantWrite is the shared body of tenant create and full update.
type tenantWrite struct {
	Slug                   string            `json:"slug,omitempty"`
	Name                   string            `json:"name,omitempty"`
	Status                 string            `json:"status,omitempty"`
	Plan                   string            `json:"plan,omitempty"`
	DefaultRoutingPolicyID string            `json:"default_routing_policy_id,omitempty"`
	Labels                 map[string]string `json:"labels,omitempty"`
}

// handleAdminCreateTenant serves POST /admin/v1/tenants.
func (s *Server) handleAdminCreateTenant(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Tenants == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the tenant store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	var body tenantWrite
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	t := &domain.Tenant{
		ID:     domain.NewID(),
		Slug:   strings.TrimSpace(body.Slug),
		Name:   strings.TrimSpace(body.Name),
		Status: domain.Status(body.Status),
		Plan:   body.Plan,
		Labels: body.Labels,
	}
	if t.Status == "" {
		t.Status = domain.StatusActive
	}
	if err := adminsvc.ValidateTenant(t); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	if existing, err := s.repos.Tenants.GetBySlug(ctx, t.Slug); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	} else if existing != nil {
		writeError(w, domain.Errorf(domain.ErrCodeInvalidRequest,
			"tenant %q already exists", t.Slug), metaFromContext(rc, nil))
		return
	}

	saved, err := s.repos.Tenants.UpsertBySlug(ctx, t)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.DefaultRoutingPolicyID != "" {
		if err := s.repos.Tenants.SetDefaultPolicy(ctx, saved.ID, body.DefaultRoutingPolicyID); err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		saved.DefaultRoutingPolicyID = body.DefaultRoutingPolicyID
	}

	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceTenant, saved.ID, nil, tenantAfter(saved))
	s.reloadRuntime(ctx)
	// Provisioning sequence: DB state is committed above; retire any stale
	// platform entries, then prewarm this tenant's hot keys.
	s.platTenantChanged(ctx, saved.ID)
	s.platTenantPrewarm(ctx, saved.ID)

	writeJSON(w, http.StatusCreated, saved)
}

// handleAdminUpdateTenant serves PUT /admin/v1/tenants/{id}: full replacement.
// The slug is immutable (it is the natural key).
func (s *Server) handleAdminUpdateTenant(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Tenants == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the tenant store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	existing, err := s.repos.Tenants.GetByID(ctx, chiURLParam(r, "id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "tenant not found"),
			metaFromContext(rc, nil))
		return
	}

	var body tenantWrite
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.Slug != "" && body.Slug != existing.Slug {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"tenant slug is immutable; delete and recreate to rename"),
			metaFromContext(rc, nil))
		return
	}
	before := tenantAfter(existing)

	updated := *existing
	if body.Name != "" {
		updated.Name = strings.TrimSpace(body.Name)
	}
	if body.Status != "" {
		updated.Status = domain.Status(body.Status)
	}
	// PUT is a full replacement: an absent plan clears the field.
	updated.Plan = body.Plan
	if body.Labels != nil {
		updated.Labels = body.Labels
	}
	if err := adminsvc.ValidateTenant(&updated); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if err := s.repos.Tenants.Update(ctx, &updated); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.DefaultRoutingPolicyID != "" {
		if err := s.repos.Tenants.SetDefaultPolicy(ctx, updated.ID, body.DefaultRoutingPolicyID); err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		updated.DefaultRoutingPolicyID = body.DefaultRoutingPolicyID
	}

	action := domain.AuditUpdate
	if body.Status != "" {
		if updated.Status == domain.StatusDisabled {
			action = domain.AuditDisable
		} else if existing.Status == domain.StatusDisabled && updated.Status == domain.StatusActive {
			action = domain.AuditEnable
		}
	}
	s.audit(ctx, rc, action, domain.ResourceTenant, updated.ID, before, tenantAfter(&updated))
	s.reloadRuntime(ctx)
	s.platTenantChanged(ctx, updated.ID)

	writeJSON(w, http.StatusOK, &updated)
}

// handleAdminDeleteTenant serves DELETE /admin/v1/tenants/{id}.
//
// A tenant holding active API keys is refused unless ?force=true: deleting it
// would orphan client configurations with no error pointing at the cause.
func (s *Server) handleAdminDeleteTenant(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Tenants == nil || s.repos.APIKeys == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the tenant store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	existing, err := s.repos.Tenants.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "tenant not found"),
			metaFromContext(rc, nil))
		return
	}

	keys, err := s.repos.APIKeys.ListByTenant(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	active := 0
	for _, k := range keys {
		if k.Status == domain.APIKeyActive {
			active++
		}
	}
	if active > 0 && r.URL.Query().Get("force") != "true" {
		writeError(w, domain.Errorf(domain.ErrCodeInvalidRequest,
			"tenant holds %d active api keys; revoke them or retry with ?force=true", active),
			metaFromContext(rc, nil))
		return
	}

	if err := s.repos.Tenants.Delete(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditDelete, domain.ResourceTenant, id, tenantAfter(existing), map[string]any{
		"keys_removed": len(keys),
	})
	s.reloadRuntime(ctx)
	s.platTenantChanged(ctx, id)

	writeJSON(w, http.StatusOK, map[string]any{"deleted": id, "keys_removed": len(keys)})
}

// ---------------------------------------------------------------------------
// API keys: update and rotation
// ---------------------------------------------------------------------------

// keyPatch is the body of PATCH /admin/v1/keys/{id}. ExpiresAt is a string
// pointer so null clears the expiry while absence leaves it unchanged.
type keyPatch struct {
	Name            *string  `json:"name,omitempty"`
	Scopes          []string `json:"scopes,omitempty"`
	ExpiresAt       *string  `json:"expires_at,omitempty"`
	RoutingPolicyID *string  `json:"routing_policy_id,omitempty"`
}

// handleAdminUpdateKey serves PATCH /admin/v1/keys/{id}.
func (s *Server) handleAdminUpdateKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.APIKeys == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the key store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	existing, err := s.repos.APIKeys.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil || existing.Status != domain.APIKeyActive {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "active api key not found"),
			metaFromContext(rc, nil))
		return
	}

	var body keyPatch
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	before := map[string]any{"name": existing.Name, "scopes": existing.Scopes}

	updated := *existing
	if body.Name != nil {
		updated.Name = *body.Name
	}
	if body.Scopes != nil {
		scopes, err := adminsvc.NormalizeKeyScopes(body.Scopes)
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		updated.Scopes = scopes
	}
	if body.ExpiresAt != nil {
		if *body.ExpiresAt == "" {
			updated.ExpiresAt = nil
		} else {
			expiry, err := time.Parse(time.RFC3339, *body.ExpiresAt)
			if err != nil {
				writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
					"expires_at must be RFC3339 (or null to clear)"), metaFromContext(rc, nil))
				return
			}
			updated.ExpiresAt = &expiry
		}
	}
	if body.RoutingPolicyID != nil {
		updated.RoutingPolicyID = *body.RoutingPolicyID
	}

	saved, err := s.repos.APIKeys.Update(ctx, &updated)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceAPIKey, id, before, map[string]any{
		"name": saved.Name, "scopes": saved.Scopes,
	})

	writeJSON(w, http.StatusOK, saved)
}

// handleAdminRotateKey serves POST /admin/v1/keys/{id}/rotate.
//
// The key keeps its identity, name, scopes and tenant; only the secret
// changes. The plaintext is returned exactly once, like creation.
func (s *Server) handleAdminRotateKey(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.APIKeys == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the key store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	existing, err := s.repos.APIKeys.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil || existing.Status != domain.APIKeyActive {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "active api key not found"),
			metaFromContext(rc, nil))
		return
	}

	generated, err := auth.GenerateKey(s.config.Auth.KeyPrefix)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	saved, err := s.repos.APIKeys.Rotate(ctx, id, generated.Prefix, generated.Hash)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	// The old secret must stop working now, not after the credential TTL.
	if err := s.auth.InvalidateHash(ctx, existing.KeyHash); err != nil {
		s.logger.Warn("failed to invalidate a cached credential", "key_id", id, "error", err)
	}

	s.audit(ctx, rc, domain.AuditRotate, domain.ResourceAPIKey, id, map[string]any{
		"prefix": existing.Prefix,
	}, map[string]any{
		"name": saved.Name, "prefix": saved.Prefix,
	})

	writeJSON(w, http.StatusCreated, map[string]any{
		"key":       saved,
		"plaintext": generated.Plaintext,
		"warning":   "store this key now; it cannot be retrieved again",
	})
}

// ---------------------------------------------------------------------------
// Policies: explicit creation
// ---------------------------------------------------------------------------

// handleAdminCreatePolicy serves POST /admin/v1/policies: creation with a
// duplicate guard, where PUT remains the idempotent upsert.
func (s *Server) handleAdminCreatePolicy(w http.ResponseWriter, r *http.Request) {
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
	if err := adminsvc.ValidatePolicy(&policy); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if policy.ID == "" {
		policy.ID = domain.NewID()
	}
	policy.ManagedBy = domain.ManagedByAPI
	if p := principal(ctx); p != nil {
		policy.CreatedBy = p.Label()
	}

	existing, err := s.repos.Policies.ListPolicies(ctx)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	for _, p := range existing {
		if p.Name == policy.Name && p.TenantID == policy.TenantID {
			writeError(w, domain.Errorf(domain.ErrCodeInvalidRequest,
				"policy %q already exists; use PUT to update it", policy.Name),
				metaFromContext(rc, nil))
			return
		}
	}

	saved, err := s.repos.Policies.Upsert(ctx, &policy)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if err := s.policies.Refresh(ctx); err != nil {
		s.logger.Warn("failed to refresh policies after a create", "error", err)
	}

	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceRoutingPolicy, saved.ID, nil, map[string]any{
		"name": saved.Name, "strategy": string(saved.Strategy), "version": saved.Version,
	})
	s.reloadRuntime(ctx)

	writeJSON(w, http.StatusCreated, saved)
}

// ---------------------------------------------------------------------------
// Overrides and endpoints
// ---------------------------------------------------------------------------

// handleAdminListOverrides serves GET /admin/v1/overrides.
func (s *Server) handleAdminListOverrides(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Overrides == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the override store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	overrides, err := s.repos.Overrides.List(ctx, parseIntParam(r, "limit", 100))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"overrides": overrides})
}

// overrideWrite is the body of POST /admin/v1/overrides.
type overrideWrite struct {
	Kind      string  `json:"kind"`
	Target    string  `json:"target,omitempty"`
	TenantID  string  `json:"tenant_id,omitempty"`
	Enabled   bool    `json:"enabled"`
	Reason    string  `json:"reason,omitempty"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// handleAdminCreateOverride serves POST /admin/v1/overrides: a generic health
// or routing override. An override is an event row; revoking one means writing
// the inverse row, so history is never rewritten.
func (s *Server) handleAdminCreateOverride(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Overrides == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the override store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	var body overrideWrite
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if strings.TrimSpace(body.Kind) == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "'kind' is required"),
			metaFromContext(rc, nil))
		return
	}

	o := &domain.AuditOverride{
		ID:       domain.NewID(),
		TenantID: body.TenantID,
		Kind:     strings.TrimSpace(body.Kind),
		Target:   body.Target,
		Enabled:  body.Enabled,
		Reason:   body.Reason,
		Actor:    actorLabel(ctx),
	}
	if body.ExpiresAt != nil && *body.ExpiresAt != "" {
		expiry, err := time.Parse(time.RFC3339, *body.ExpiresAt)
		if err != nil {
			writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
				"expires_at must be RFC3339"), metaFromContext(rc, nil))
			return
		}
		o.ExpiresAt = &expiry
	}

	if err := s.repos.Overrides.Create(ctx, o); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditOverridden, domain.ResourceOverride, o.ID, nil, map[string]any{
		"kind": o.Kind, "target": o.Target, "enabled": o.Enabled,
	})
	s.reloadRuntime(ctx)

	writeJSON(w, http.StatusCreated, o)
}

// handleAdminDeleteEndpoint serves DELETE /admin/v1/endpoints/{id}.
func (s *Server) handleAdminDeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Endpoints == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the endpoint store is unavailable"),
			metaFromContext(rc, nil))
		return
	}

	id := chiURLParam(r, "id")
	existing, err := s.repos.Endpoints.GetByID(ctx, id)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if existing == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "endpoint not found"),
			metaFromContext(rc, nil))
		return
	}

	if err := s.repos.Endpoints.Delete(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	s.audit(ctx, rc, domain.AuditDelete, domain.ResourceEndpoint, id, map[string]any{
		"slug": existing.Slug,
	}, nil)
	s.reloadRuntime(ctx)
	s.cacheFlushBestEffort(ctx, domain.CacheScopeTenant, existing.TenantID, existing.Slug, domain.CacheInvalidateEndpointChange, actorLabel(ctx))

	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}
