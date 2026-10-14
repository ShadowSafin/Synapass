// Package api benchmark surface: the Autonomous Benchmark Engine's REST API.
//
// Every route is gated on the master switch: when the engine is disabled all
// routes report the feature as disabled (HTTP 404 with a stable code) so the
// dashboard can hide itself and no worker, scheduler or queue traffic exists.
// When enabled, all rows are workspace scoped by tenant_id.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/shadowsafin/synapass/internal/benchmark"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/storage"
)

// benchmarkTenant resolves the workspace scope: explicit tenant_id wins,
// otherwise the caller's own tenant, otherwise global.
//
// Non-UUID values (notably the synthetic "system" tenant of the bootstrap
// admin key) collapse to global scope: passing them through would break the
// storage layer's UUID casts and leak a 500 for what is a valid admin view.
func (s *Server) benchmarkTenant(r *http.Request) string {
	id := strings.TrimSpace(r.URL.Query().Get("tenant_id"))
	if id == "" {
		if p := principal(r.Context()); p != nil {
			id = p.TenantID()
		} else if rc := requestContext(r.Context()); rc != nil && rc.Tenant != nil {
			id = rc.Tenant.ID
		}
	}
	if id == "" {
		return ""
	}
	return sanitizeBenchmarkTenant(id)
}

// sanitizeBenchmarkTenant collapses non-UUID tenant identifiers to global
// scope, guarding request bodies as well as query parameters.
func sanitizeBenchmarkTenant(id string) string {
	if id == "" {
		return ""
	}
	if _, err := uuid.Parse(id); err != nil {
		return ""
	}
	return id
}

// requireBenchmark gates every benchmark route on the feature flag.
func (s *Server) requireBenchmark(w http.ResponseWriter, r *http.Request) *benchmark.Engine {
	rc := requestContext(r.Context())
	if s.benchmark == nil || !s.benchmark.Enabled() {
		err := domain.NewError(domain.ErrCodeNotFound, "the benchmark engine is disabled (set ENABLE_BENCHMARK_ENGINE=true)")
		err.Status = http.StatusNotFound
		writeError(w, err, metaFromContext(rc, nil))
		return nil
	}
	return s.benchmark
}

// handleBenchmarkStatus serves GET /admin/v1/benchmark/status.
// Unlike the other routes it is always registered: the dashboard calls it to
// decide whether to show the Benchmark page, so it must answer (with
// enabled=false) even when the engine is off.
func (s *Server) handleBenchmarkStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 5*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	enabled := s.benchmark != nil && s.benchmark.Enabled()
	status := map[string]any{"enabled": enabled}
	if s.benchmark != nil {
		status["config_enabled"] = s.benchmark.ConfigEnabled()
	}
	if enabled {
		if ov, err := s.benchmark.Overview(ctx, s.benchmarkTenant(r)); err == nil {
			status["engine_status"] = ov.EngineStatus
			status["scheduler_status"] = ov.SchedulerStatus
		} else {
			status["engine_status"] = "running"
			status["scheduler_status"] = "running"
		}
	} else {
		status["engine_status"] = "disabled"
		status["scheduler_status"] = "disabled"
	}
	_ = rc
	writeJSON(w, http.StatusOK, status)
}

// handleBenchmarkSetEnabled serves POST /admin/v1/benchmark/enabled.
//
// This is the dashboard toggle: it flips the persisted runtime switch and
// starts (or stops) the scheduler and workers immediately, so no restart is
// needed. It is deliberately NOT gated on requireBenchmark — gating it would
// make enabling from a disabled state impossible. It still requires full
// admin scope (POST defaults to ScopeAdminAll) and is audited.
func (s *Server) handleBenchmarkSetEnabled(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.benchmark == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the benchmark engine is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}

	actor := ""
	if p := principal(ctx); p != nil {
		actor = p.Label()
	}
	// The toggle applies in memory immediately; persistence is best-effort so
	// a missing settings store degrades to "until next restart" rather than
	// refusing the operator's explicit action.
	if s.repos != nil && s.repos.Settings != nil {
		if err := s.repos.Settings.Set(ctx, "benchmark_enabled", body.Enabled, actor); err != nil {
			s.logger.Warn("benchmark toggle persistence failed; the switch lasts until restart",
				"error", err)
		}
	}
	s.benchmark.SetRuntimeEnabled(body.Enabled)
	if body.Enabled {
		s.benchmark.Start(context.Background())
	} else if !s.benchmark.ConfigEnabled() {
		// Only stop when configuration does not force the engine on;
		// otherwise Stop would be immediately contradicted by Enabled().
		s.benchmark.Stop()
	}

	action := domain.AuditUpdate
	if body.Enabled {
		action = domain.AuditCreate
	}
	s.audit(ctx, rc, action, domain.ResourceSettings, "benchmark_enabled", nil,
		map[string]any{"enabled": body.Enabled, "actor": actor})

	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":        s.benchmark.Enabled(),
		"config_enabled": s.benchmark.ConfigEnabled(),
	})
}

// handleBenchmarkOverview serves GET /admin/v1/benchmark/overview.
func (s *Server) handleBenchmarkOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	ov, err := eng.Overview(ctx, s.benchmarkTenant(r))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, ov)
}

// benchmarkJobBody is the create/update payload for a job.
type benchmarkJobBody struct {
	Name              string            `json:"name"`
	Description       string            `json:"description"`
	Enabled           *bool             `json:"enabled"`
	Providers         []string          `json:"providers"`
	Models            []string          `json:"models"`
	DatasetID         string            `json:"dataset_id"`
	Schedule          string            `json:"schedule"`
	CronExpression    string            `json:"cron_expression"`
	TimeoutMS         int64             `json:"timeout_ms"`
	RetryCount        int               `json:"retry_count"`
	Region            string            `json:"region"`
	Concurrency       int               `json:"concurrency"`
	MaxDailyBudgetUSD float64           `json:"max_daily_budget_usd"`
	Tags              map[string]string `json:"tags"`
	TenantID          string            `json:"tenant_id"`
}

// handleBenchmarkListJobs serves GET /admin/v1/benchmark/jobs.
func (s *Server) handleBenchmarkListJobs(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.requireBenchmark(w, r) == nil {
		return
	}
	jobs, err := s.benchmark.ListJobs(ctx, s.benchmarkTenant(r), parseIntParam(r, "limit", 100), parseIntParam(r, "offset", 0))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if jobs == nil {
		jobs = []domain.BenchmarkJob{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": jobs})
}

// handleBenchmarkCreateJob serves POST /admin/v1/benchmark/jobs.
func (s *Server) handleBenchmarkCreateJob(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	var body benchmarkJobBody
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	tenantID := sanitizeBenchmarkTenant(firstNonEmptyString(body.TenantID, s.benchmarkTenant(r)))
	job := &domain.BenchmarkJob{
		Name: body.Name, Description: body.Description,
		Providers: body.Providers, Models: body.Models, DatasetID: body.DatasetID,
		Schedule: body.Schedule, CronExpression: body.CronExpression,
		TimeoutMS: body.TimeoutMS, RetryCount: body.RetryCount, Region: body.Region,
		Concurrency: body.Concurrency, MaxDailyBudgetUSD: body.MaxDailyBudgetUSD, Tags: body.Tags,
	}
	saved, err := eng.CreateJob(ctx, tenantID, job)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditCreate, domain.ResourceBenchmarkJob, saved.ID, nil, benchmarkJobAfter(saved))
	writeJSON(w, http.StatusCreated, saved)
}

// handleBenchmarkUpdateJob serves PUT /admin/v1/benchmark/jobs/{id}.
func (s *Server) handleBenchmarkUpdateJob(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	var body benchmarkJobBody
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	job, gerr := s.benchmarkJobByID(ctx, chiURLParam(r, "id"))
	if gerr != nil || job == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "benchmark job not found"), metaFromContext(rc, nil))
		return
	}
	before := benchmarkJobAfter(job)
	if body.Name != "" {
		job.Name = body.Name
	}
	job.Description = body.Description
	if body.Enabled != nil {
		job.Enabled = *body.Enabled
	}
	if body.Providers != nil {
		job.Providers = body.Providers
	}
	if body.Models != nil {
		job.Models = body.Models
	}
	if body.DatasetID != "" {
		job.DatasetID = body.DatasetID
	}
	if body.Schedule != "" {
		job.Schedule = body.Schedule
	}
	if body.CronExpression != "" || body.Schedule == "custom" {
		job.CronExpression = body.CronExpression
	}
	if body.TimeoutMS > 0 {
		job.TimeoutMS = body.TimeoutMS
	}
	if body.RetryCount >= 0 {
		job.RetryCount = body.RetryCount
	}
	if body.Region != "" {
		job.Region = body.Region
	}
	if body.Concurrency > 0 {
		job.Concurrency = body.Concurrency
	}
	if body.MaxDailyBudgetUSD >= 0 {
		job.MaxDailyBudgetUSD = body.MaxDailyBudgetUSD
	}
	if body.Tags != nil {
		job.Tags = body.Tags
	}
	updated, err := eng.UpdateJob(ctx, s.benchmarkTenant(r), job)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceBenchmarkJob, updated.ID, before, benchmarkJobAfter(updated))
	writeJSON(w, http.StatusOK, updated)
}

// handleBenchmarkDeleteJob serves DELETE /admin/v1/benchmark/jobs/{id}.
func (s *Server) handleBenchmarkDeleteJob(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	id := chiURLParam(r, "id")
	if err := eng.DeleteJob(ctx, s.benchmarkTenant(r), id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditDelete, domain.ResourceBenchmarkJob, id, nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// handleBenchmarkRun serves POST /admin/v1/benchmark/run.
func (s *Server) handleBenchmarkRun(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	var body struct {
		JobID    string `json:"job_id"`
		TenantID string `json:"tenant_id"`
	}
	if err := decodeJSONBody(r, 1<<16, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if body.JobID == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "job_id is required"), metaFromContext(rc, nil))
		return
	}
	run, err := eng.TriggerRun(ctx, sanitizeBenchmarkTenant(firstNonEmptyString(body.TenantID, s.benchmarkTenant(r))), body.JobID)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

// handleBenchmarkRuns serves GET /admin/v1/benchmark/runs.
func (s *Server) handleBenchmarkRuns(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.requireBenchmark(w, r) == nil {
		return
	}
	runs, err := s.benchmark.ListRuns(ctx, s.benchmarkTenant(r), r.URL.Query().Get("job_id"),
		parseIntParam(r, "limit", 50), parseIntParam(r, "offset", 0))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if runs == nil {
		runs = []domain.BenchmarkRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// handleBenchmarkHistory serves GET /admin/v1/benchmark/history.
func (s *Server) handleBenchmarkHistory(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	q := r.URL.Query()
	filter, err := benchmarkHistoryFilter(r, s.benchmarkTenant(r))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	_ = q
	results, err := eng.History(ctx, filter)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if results == nil {
		results = []domain.BenchmarkResult{}
	}
	// Optional export: ?export=csv|json|excel|pdf downloads the same rows.
	if format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("export"))); format != "" {
		raw, contentType, err := benchmark.ExportResults(results, benchmark.ExportFormat(format))
		if err != nil {
			writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, err.Error()), metaFromContext(rc, nil))
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", "attachment; filename=benchmark-export."+format)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

// handleBenchmarkReport serves GET /admin/v1/benchmark/report.
func (s *Server) handleBenchmarkReport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	period := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("period")))
	if period == "" {
		period = "daily"
	}
	if r.URL.Query().Get("generate") == "true" {
		rep, err := eng.GenerateReport(ctx, s.benchmarkTenant(r), period)
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		s.audit(ctx, rc, domain.AuditCreate, domain.ResourceBenchmarkReport, rep.ID, nil, nil)
		writeJSON(w, http.StatusCreated, rep)
		return
	}
	reports, err := eng.Reports(ctx, s.benchmarkTenant(r), parseIntParam(r, "limit", 20))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if reports == nil {
		reports = []domain.BenchmarkReport{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"reports": reports})
}

// handleBenchmarkLive serves GET /admin/v1/benchmark/live (SSE).
func (s *Server) handleBenchmarkLive(w http.ResponseWriter, r *http.Request) {
	rc := requestContext(r.Context())
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "streaming is not supported"), metaFromContext(rc, nil))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	ch := eng.Subscribe()
	defer eng.Unsubscribe(ch)
	// Initial hello so the dashboard shows "live" immediately.
	_, _ = w.Write([]byte("event: hello\ndata: {\"live\":true}\n\n"))
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			raw, _ := json.Marshal(ev)
			_, _ = w.Write([]byte("data: " + string(raw) + "\n\n"))
			flusher.Flush()
		case <-time.After(15 * time.Second):
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		}
	}
}

// handleBenchmarkProviders serves GET /admin/v1/benchmark/providers.
func (s *Server) handleBenchmarkProviders(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.requireBenchmark(w, r) == nil {
		return
	}
	if s.repos == nil || s.repos.Providers == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the provider store is unavailable"), metaFromContext(rc, nil))
		return
	}
	providers, err := s.repos.Providers.List(ctx)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	names := make([]string, 0, len(providers))
	for _, p := range providers {
		names = append(names, p.Name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": names})
}

// handleBenchmarkModels serves GET /admin/v1/benchmark/models?provider=.
func (s *Server) handleBenchmarkModels(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	if s.requireBenchmark(w, r) == nil {
		return
	}
	if s.repos == nil || s.repos.Models == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the model store is unavailable"), metaFromContext(rc, nil))
		return
	}
	provider := strings.TrimSpace(r.URL.Query().Get("provider"))
	var models []domain.Model
	var err error
	if provider != "" && s.repos.Providers != nil {
		var p *domain.Provider
		if p, err = s.repos.Providers.GetByName(ctx, provider); err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		if p == nil {
			writeJSON(w, http.StatusOK, map[string]any{"models": []domain.Model{}})
			return
		}
		models, err = s.repos.Models.ListByProvider(ctx, p.ID)
	} else {
		models, err = s.repos.Models.List(ctx)
	}
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if models == nil {
		models = []domain.Model{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// handleBenchmarkHealth serves GET /admin/v1/benchmark/health.
func (s *Server) handleBenchmarkHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	from, _ := parseTimeParam(r.URL.Query().Get("from"), time.Now().UTC())
	to, _ := parseTimeParam(r.URL.Query().Get("to"), time.Now().UTC())
	health, err := eng.Health(ctx, s.benchmarkTenant(r), from, to)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if health == nil {
		health = []domain.BenchmarkProviderHealth{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"health": health})
}

// handleBenchmarkCompare serves GET /admin/v1/benchmark/compare?providers=a,b.
func (s *Server) handleBenchmarkCompare(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	var providers []string
	for _, p := range strings.Split(r.URL.Query().Get("providers"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			providers = append(providers, p)
		}
	}
	from, _ := parseTimeParam(r.URL.Query().Get("from"), time.Now().UTC())
	to, _ := parseTimeParam(r.URL.Query().Get("to"), time.Now().UTC())
	comp, err := eng.Comparison(ctx, s.benchmarkTenant(r), providers, from, to)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"comparison": comp})
}

// handleBenchmarkInsights serves GET /admin/v1/benchmark/insights.
func (s *Server) handleBenchmarkInsights(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	insights, err := eng.Insights(ctx, s.benchmarkTenant(r))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"insights": insights})
}

// handleBenchmarkDatasets serves GET/POST /admin/v1/benchmark/datasets.
func (s *Server) handleBenchmarkDatasets(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		datasets, err := eng.ListDatasets(ctx, s.benchmarkTenant(r))
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"datasets": datasets})
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			TenantID    string `json:"tenant_id"`
			Prompts     []struct {
				Prompt      string  `json:"prompt"`
				Category    string  `json:"category"`
				Difficulty  string  `json:"difficulty"`
				Weight      float64 `json:"weight"`
				MaxTokens   int     `json:"max_tokens"`
				Temperature float64 `json:"temperature"`
			} `json:"prompts"`
		}
		if err := decodeJSONBody(r, 1<<20, &body); err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		if strings.TrimSpace(body.Name) == "" {
			writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "dataset name is required"), metaFromContext(rc, nil))
			return
		}
		var prompts []domain.BenchmarkPrompt
		for _, p := range body.Prompts {
			prompts = append(prompts, domain.BenchmarkPrompt{
				Prompt: p.Prompt, Category: domain.BenchmarkPromptCategory(orDefault(p.Category, "reasoning")),
				Difficulty: domain.BenchmarkPromptDifficulty(orDefault(p.Difficulty, "medium")),
				Weight:     orFloat(p.Weight, 1), MaxTokens: orInt(p.MaxTokens, 512), Temperature: p.Temperature,
			})
		}
		ds, err := eng.CreateDataset(ctx, sanitizeBenchmarkTenant(firstNonEmptyString(body.TenantID, s.benchmarkTenant(r))), body.Name, body.Description, prompts)
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		s.audit(ctx, rc, domain.AuditCreate, domain.ResourceBenchmarkDataset, ds.ID, nil, nil)
		writeJSON(w, http.StatusCreated, ds)
	default:
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "method not allowed"), metaFromContext(rc, nil))
	}
}

// handleBenchmarkNotifications serves GET/POST /admin/v1/benchmark/notifications.
func (s *Server) handleBenchmarkNotifications(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 10*time.Second)
	defer cancel()
	rc := requestContext(ctx)
	eng := s.requireBenchmark(w, r)
	if eng == nil {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rules, err := eng.ListNotifications(ctx, s.benchmarkTenant(r))
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"notifications": rules})
	case http.MethodPost:
		var body struct {
			Channel  string   `json:"channel"`
			Target   string   `json:"target"`
			Events   []string `json:"events"`
			Enabled  *bool    `json:"enabled"`
			TenantID string   `json:"tenant_id"`
		}
		if err := decodeJSONBody(r, 1<<20, &body); err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		rule, err := eng.UpsertNotification(ctx, sanitizeBenchmarkTenant(firstNonEmptyString(body.TenantID, s.benchmarkTenant(r))),
			domain.BenchmarkNotificationChannel(body.Channel), body.Target, body.Events, enabled)
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		s.audit(ctx, rc, domain.AuditCreate, domain.ResourceBenchmarkNotification, rule.ID, nil, nil)
		writeJSON(w, http.StatusCreated, rule)
	default:
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest, "method not allowed"), metaFromContext(rc, nil))
	}
}

// benchmarkHistoryFilter builds the history query from request parameters,
// supporting last-hour through last-year presets plus custom ranges.
func benchmarkHistoryFilter(r *http.Request, tenantID string) (storage.BenchmarkHistoryFilter, error) {
	q := r.URL.Query()
	f := storage.BenchmarkHistoryFilter{
		TenantID: tenantID,
		JobID:    strings.TrimSpace(q.Get("job_id")),
		Provider: strings.TrimSpace(q.Get("provider")),
		Model:    strings.TrimSpace(q.Get("model")),
		Category: strings.TrimSpace(q.Get("category")),
		Limit:    parseIntParam(r, "limit", 100),
		Offset:   parseIntParam(r, "offset", 0),
	}
	// Range presets: 1h, 24h, 7d, 30d, 90d, 1y — plus explicit from/to.
	now := time.Now().UTC()
	switch strings.ToLower(strings.TrimSpace(q.Get("range"))) {
	case "", "custom":
		from, err := parseTimeParam(q.Get("from"), now)
		if err != nil {
			return f, err
		}
		to, err := parseTimeParam(q.Get("to"), now)
		if err != nil {
			return f, err
		}
		f.From, f.To = from, to
		if f.From.IsZero() && q.Get("from") == "" && q.Get("range") == "" {
			f.From = now.Add(-24 * time.Hour)
		}
	case "1h", "hour":
		f.From = now.Add(-time.Hour)
	case "24h", "day":
		f.From = now.Add(-24 * time.Hour)
	case "7d", "week":
		f.From = now.Add(-7 * 24 * time.Hour)
	case "30d", "month":
		f.From = now.Add(-30 * 24 * time.Hour)
	case "90d":
		f.From = now.Add(-90 * 24 * time.Hour)
	case "1y", "year":
		f.From = now.Add(-365 * 24 * time.Hour)
	default:
		return f, domain.NewError(domain.ErrCodeInvalidRequest, "unknown range preset")
	}
	if raw := strings.TrimSpace(q.Get("success")); raw != "" {
		b := raw == "true" || raw == "1"
		f.Success = &b
	}
	return f, nil
}

func (s *Server) benchmarkJobByID(ctx context.Context, id string) (*domain.BenchmarkJob, error) {
	if s.repos == nil || s.repos.BenchmarkJobs == nil {
		return nil, domain.NewError(domain.ErrCodeInternal, "the benchmark store is unavailable")
	}
	return s.repos.BenchmarkJobs.GetByID(ctx, id)
}

func benchmarkJobAfter(j *domain.BenchmarkJob) map[string]any {
	raw, _ := json.Marshal(j)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func orDefault(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return v
}

func orInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func orFloat(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}
