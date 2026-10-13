package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/cost"
	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/storage"
)

// costFilter builds the shared traffic filter from query params: window plus
// every cost dimension the dashboard slices by.
func costFilter(r *http.Request, window domain.TimeRange) storage.QueryFilter {
	filter := encodeQueryFilter(r, "")
	filter.From, filter.To = window.From, window.To
	if v := r.URL.Query().Get("endpoint_id"); v != "" {
		filter.EndpointID = v
	}
	return filter
}

// costWindow reads the dashboard's time window, defaulting to the trailing
// day like every other analytics surface.
func costWindow(r *http.Request) (domain.TimeRange, error) {
	return parseTimeRange(r, 24*time.Hour)
}

// handleAdminCostOverview serves GET /admin/v1/cost/overview: window totals,
// estimate accuracy, unit rate and measured savings.
func (s *Server) handleAdminCostOverview(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	window, err := costWindow(r)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	ov, err := s.repos.Usage.CostTotals(ctx, costFilter(r, window))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	ov.EstimateAccuracy = cost.WindowAccuracy(ov.EstimatedUSD, ov.ActualUSD)
	if ov.Tokens > 0 {
		ov.CostPerKiloTokenUSD = cost.RoundMicro(ov.ActualUSD / float64(ov.Tokens) * 1000)
	}
	ov.CacheSavingsUSD = cost.RoundMicro(ov.CacheSavingsUSD)
	ov.RoutingSavingsUSD = cost.RoundMicro(ov.RoutingSavingsUSD)
	writeJSON(w, http.StatusOK, map[string]any{"overview": ov})
}

// handleAdminCostGrouped serves GET /admin/v1/cost/by?dimension=provider:
// spend slices for every first-class dimension.
func (s *Server) handleAdminCostGrouped(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	window, err := costWindow(r)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	dimension := r.URL.Query().Get("dimension")
	if dimension == "" {
		dimension = "provider"
	}
	rows, err := s.repos.Usage.CostGrouped(ctx, costFilter(r, window), dimension)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if rows == nil {
		rows = []domain.CostDimensionRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"dimension": dimension, "rows": rows})
}

// handleAdminCostSeries serves GET /admin/v1/cost/series: actual vs estimated
// spend over time for the trend charts.
func (s *Server) handleAdminCostSeries(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	window, err := costWindow(r)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	points, err := s.repos.Usage.CostSeries(ctx, costFilter(r, window), r.URL.Query().Get("bucket"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"series": points})
}

// handleAdminCostTop serves GET /admin/v1/cost/requests/top: the window's
// most expensive requests, with their breakdowns attached.
func (s *Server) handleAdminCostTop(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	window, err := costWindow(r)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	rows, err := s.repos.Usage.TopCostRequests(ctx, costFilter(r, window), parseIntParam(r, "limit", 20))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": rows})
}

// handleAdminCostRequest serves GET /admin/v1/cost/requests/{requestID}: the
// request-level cost trace — what it cost, why, and under which sheet.
func (s *Server) handleAdminCostRequest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	rec, err := s.repos.Usage.GetByRequestID(ctx, chiURLParam(r, "requestID"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	if rec == nil {
		writeError(w, domain.NewError(domain.ErrCodeNotFound, "no cost record for that request"),
			metaFromContext(rc, nil))
		return
	}
	accuracy := cost.Accuracy(rec.EstimateCost.USD, rec.Cost.USD)
	writeJSON(w, http.StatusOK, map[string]any{
		"record":            rec,
		"estimate_accuracy": accuracy,
	})
}

// handleAdminPricingList serves GET /admin/v1/cost/pricing: the versioned
// sheets, newest first.
func (s *Server) handleAdminPricingList(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Pricing == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the pricing store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	scope := domain.PricingScope(r.URL.Query().Get("scope"))
	if scope != "" && !scope.Valid() {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"scope must be one of global, provider, model, tenant"), metaFromContext(rc, nil))
		return
	}
	versions, err := s.repos.Pricing.List(ctx, scope, parseIntParam(r, "limit", 100))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	cost.SortVersions(versions)
	writeJSON(w, http.StatusOK, map[string]any{"pricing": versions})
}

// pricingCreateBody is the validated shape for minting a price sheet.
type pricingCreateBody struct {
	Scope                     string     `json:"scope"`
	ScopeID                   string     `json:"scope_id"`
	Currency                  string     `json:"currency"`
	InputCostPerMillion       float64    `json:"input_cost_per_million"`
	OutputCostPerMillion      float64    `json:"output_cost_per_million"`
	CachedInputCostPerMillion float64    `json:"cached_input_cost_per_million"`
	BaseFeeUSD                float64    `json:"base_fee_usd"`
	EffectiveFrom             *time.Time `json:"effective_from"`
}

// handleAdminPricingCreate serves POST /admin/v1/cost/pricing: mint one
// immutable sheet. There is no update endpoint on purpose — a price change
// is a new row, which is what keeps history reproducible.
func (s *Server) handleAdminPricingCreate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Pricing == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the pricing store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	var body pricingCreateBody
	if err := decodeJSONBody(r, 1<<20, &body); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	scope := domain.PricingScope(body.Scope)
	if scope == "" {
		scope = domain.PricingScopeGlobal
	}
	if !scope.Valid() {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"scope must be one of global, provider, model, tenant"), metaFromContext(rc, nil))
		return
	}
	if scope != domain.PricingScopeGlobal && body.ScopeID == "" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"scope_id is required for provider, model and tenant sheets"), metaFromContext(rc, nil))
		return
	}
	for _, price := range []float64{body.InputCostPerMillion, body.OutputCostPerMillion,
		body.CachedInputCostPerMillion, body.BaseFeeUSD} {
		if price < 0 {
			writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
				"prices cannot be negative"), metaFromContext(rc, nil))
			return
		}
	}
	effective := domain.Now()
	if body.EffectiveFrom != nil && !body.EffectiveFrom.IsZero() {
		effective = body.EffectiveFrom.UTC()
	}
	v := &domain.PricingVersion{
		Scope:                     scope,
		ScopeID:                   body.ScopeID,
		Currency:                  body.Currency,
		InputCostPerMillion:       body.InputCostPerMillion,
		OutputCostPerMillion:      body.OutputCostPerMillion,
		CachedInputCostPerMillion: body.CachedInputCostPerMillion,
		BaseFeeUSD:                body.BaseFeeUSD,
		EffectiveFrom:             effective,
		CreatedBy:                 actorLabel(ctx),
	}
	if err := s.repos.Pricing.Create(ctx, v); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditCreate, domain.ResourcePricingVersion, v.ID, nil, map[string]any{
		"scope": string(scope), "scope_id": body.ScopeID,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"pricing": v})
}

// handleAdminBudgetsStatus serves GET /admin/v1/cost/budgets: every budget
// evaluated against authoritative usage spend, with new threshold crossings
// fired exactly once as a side effect of the read.
//
// Evaluation is lazy — computed when the dashboard asks, not on a timer —
// because spend only moves on traffic and the numbers are always fresh at
// read time. A background reconciler can call this same path later without
// changing its semantics.
func (s *Server) handleAdminBudgetsStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 30*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Budgets == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the budget store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	now := domain.Now()
	budgets, err := s.repos.Budgets.ListByTenant(ctx, r.URL.Query().Get("tenant_id"))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	statuses := make([]domain.BudgetStatus, 0, len(budgets))
	for _, b := range budgets {
		start := periodStartOf(b.Period, now)
		filter := storage.QueryFilter{TenantID: b.TenantID, From: start, To: now}
		switch b.Scope {
		case "key":
			filter.APIKeyID = b.ScopeID
		case "policy":
			filter.PolicyID = b.ScopeID
		}
		spent, err := s.repos.Usage.SpendForBudget(ctx, filter)
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		st := cost.Evaluate(b, spent, b.LimitUSD, now)
		st.BudgetID = b.ID
		statuses = append(statuses, st)

		// Fire newly crossed thresholds exactly once per period.
		if len(b.AlertThresholdsUSD) > 0 && s.repos.BudgetAlerts != nil {
			fired, err := s.repos.BudgetAlerts.FiredThresholds(ctx, b.ID, start)
			if err == nil {
				for _, t := range cost.CrossedThresholds(b.AlertThresholdsUSD, spent, fired) {
					_ = s.repos.BudgetAlerts.Fire(ctx, &domain.BudgetAlert{
						BudgetID: b.ID, TenantID: b.TenantID, ThresholdUSD: t,
						SpentUSD: cost.RoundMicro(spent), LimitUSD: b.LimitUSD,
						Period: b.Period, PeriodStart: start,
					})
				}
			}
		}
	}
	alerts := []domain.BudgetAlert{}
	if s.repos.BudgetAlerts != nil {
		if recent, err := s.repos.BudgetAlerts.ListRecent(ctx, r.URL.Query().Get("tenant_id"), 50); err == nil {
			alerts = recent
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"budgets": statuses, "alerts": alerts})
}

// periodStartOf returns the start of the budget period containing now.
func periodStartOf(period domain.BudgetPeriod, now time.Time) time.Time {
	now = now.UTC()
	switch period {
	case domain.BudgetDaily:
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	case domain.BudgetWeekly:
		daysSinceMonday := (int(now.Weekday()) + 6) % 7
		day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return day.AddDate(0, 0, -daysSinceMonday)
	case domain.BudgetMonthly:
		return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	default:
		return time.Time{}
	}
}

// handleAdminCostAnomalies serves GET /admin/v1/cost/anomalies: detect fresh
// spikes across provider, model, tenant and endpoint spend, persist the new
// ones (deduped against open anomalies), and return the open set.
func (s *Server) handleAdminCostAnomalies(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 30*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil || s.repos.Anomalies == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the anomaly store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	now := domain.Now()
	filter := storage.QueryFilter{From: now.AddDate(0, 0, -16), To: now}
	if v := r.URL.Query().Get("tenant_id"); v != "" {
		filter.TenantID = v
	}
	var points []cost.SeriesPoint
	for _, dimension := range []string{"provider", "model", "tenant", "endpoint"} {
		series, err := s.repos.Usage.SpendSeriesByDimension(ctx, filter, dimension, 15, 30)
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		for _, p := range series {
			points = append(points, cost.SeriesPoint{
				Dimension: dimension, Key: p.Key, Day: p.Day, Cost: p.Cost,
			})
		}
	}
	for _, a := range cost.Detect(points, now) {
		exists, err := s.repos.Anomalies.ExistsOpen(ctx, a.Dimension, a.Key, a.WindowFrom)
		if err != nil || exists {
			continue
		}
		_ = s.repos.Anomalies.Record(ctx, &a)
	}
	open, err := s.repos.Anomalies.ListOpen(ctx, 50)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"anomalies": open})
}

// handleAdminCostAnomalyResolve serves POST /admin/v1/cost/anomalies/{id}/resolve.
func (s *Server) handleAdminCostAnomalyResolve(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Anomalies == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the anomaly store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	id := chiURLParam(r, "id")
	if err := s.repos.Anomalies.Resolve(ctx, id); err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	s.audit(ctx, rc, domain.AuditUpdate, domain.ResourceCostAnomaly, id, nil, map[string]any{
		"resolved": true,
	})
	writeJSON(w, http.StatusOK, map[string]any{"resolved": id})
}

// handleAdminCostForecast serves GET /admin/v1/cost/forecast: the month-end
// projection from observed daily spend.
func (s *Server) handleAdminCostForecast(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	window, err := costWindow(r)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	filter := costFilter(r, window)
	daily, err := s.repos.Usage.SpendByDay(ctx, filter, "", "")
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	var days []cost.DaySpend
	for _, p := range daily {
		days = append(days, cost.DaySpend{Day: p.Bucket, Cost: p.ActualUSD})
	}
	writeJSON(w, http.StatusOK, map[string]any{"forecast": cost.Forecast(days, domain.Now())})
}

// handleAdminCostSavings serves GET /admin/v1/cost/savings: measured avoided
// spend by source, each computed from recorded rows.
func (s *Server) handleAdminCostSavings(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 15*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	window, err := costWindow(r)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	ov, err := s.repos.Usage.CostTotals(ctx, costFilter(r, window))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	fallbackUSD, err := s.repos.Usage.FallbackSavingsTotal(ctx, costFilter(r, window))
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	savings := cost.Savings{FallbackUSD: cost.RoundMicro(fallbackUSD)}
	savings.CacheUSD = cost.RoundMicro(ov.CacheSavingsUSD)
	savings.RoutingUSD = cost.RoundMicro(ov.RoutingSavingsUSD)
	writeJSON(w, http.StatusOK, map[string]any{
		"savings":   savings,
		"total_usd": savings.Total(),
	})
}

// handleAdminCostExport serves GET /admin/v1/cost/export?format=csv|json: the
// window's billing rows for finance. Paged server-side and capped, because
// an unbounded export is how a dashboard takes down a database.
func (s *Server) handleAdminCostExport(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := timeoutContext(r, 60*time.Second)
	defer cancel()
	rc := requestContext(ctx)

	if s.repos == nil || s.repos.Usage == nil {
		writeError(w, domain.NewError(domain.ErrCodeInternal, "the usage store is unavailable"),
			metaFromContext(rc, nil))
		return
	}
	window, err := costWindow(r)
	if err != nil {
		writeError(w, err, metaFromContext(rc, nil))
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		writeError(w, domain.NewError(domain.ErrCodeInvalidRequest,
			"format must be csv or json"), metaFromContext(rc, nil))
		return
	}
	filter := costFilter(r, window)
	var rows []domain.UsageRecord
	for offset := 0; offset < 5000; offset += 1000 {
		filter.Limit, filter.Offset = 1000, offset
		page, err := s.repos.Usage.List(ctx, filter)
		if err != nil {
			writeError(w, err, metaFromContext(rc, nil))
			return
		}
		rows = append(rows, page...)
		if len(page) < 1000 {
			break
		}
	}

	stamp := window.To.Format("20060102-150405")
	if format == "json" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="synapass-cost-%s.json"`, stamp))
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows})
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="synapass-cost-%s.csv"`, stamp))
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"request_id", "created_at", "tenant_id", "provider", "model",
		"endpoint_id", "request_type", "prompt_tokens", "completion_tokens", "cached_tokens",
		"actual_usd", "estimated_usd", "pricing_source", "outcome", "cache_hit", "fallback_used"})
	for _, rec := range rows {
		_ = cw.Write([]string{
			rec.RequestID.String(), rec.CreatedAt.Format(time.RFC3339), rec.TenantID,
			rec.Provider, rec.Model, rec.EndpointID, string(rec.RequestType),
			strconv.Itoa(rec.Usage.PromptTokens), strconv.Itoa(rec.Usage.CompletionTokens),
			strconv.Itoa(rec.Usage.CachedPromptTokens),
			strconv.FormatFloat(rec.Cost.USD, 'f', 6, 64),
			strconv.FormatFloat(rec.EstimateCost.USD, 'f', 6, 64),
			rec.PricingSource, string(rec.Outcome),
			strconv.FormatBool(rec.CacheHit), strconv.FormatBool(rec.FallbackUsed),
		})
	}
	cw.Flush()
}
