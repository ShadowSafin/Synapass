package domain

import "time"

// PricingScope selects which entity a pricing version overrides.
//
// Resolution precedence is fixed: tenant beats model beats provider beats
// global. The order is a property of the billing model, not of insertion
// order, so two operators describing the same overrides in different orders
// get the same invoice.
type PricingScope string

const (
	PricingScopeGlobal   PricingScope = "global"
	PricingScopeProvider PricingScope = "provider"
	PricingScopeModel    PricingScope = "model"
	PricingScopeTenant   PricingScope = "tenant"
)

// Valid reports whether the scope is one Synapass resolves.
func (s PricingScope) Valid() bool {
	switch s {
	case PricingScopeGlobal, PricingScopeProvider, PricingScopeModel, PricingScopeTenant:
		return true
	default:
		return false
	}
}

// PricingVersion is one dated price sheet entry. Prices are quoted in USD per
// one million tokens, matching the unit on provider price sheets so operators
// can copy values directly, and in USD flat per request for base fees.
//
// Versions are immutable once effective: a price change is a new row with a
// later EffectiveFrom, never an edit. That keeps every historical request
// reproducible — recalculating last month's invoice uses the rows that were
// effective last month, not today's sheet.
type PricingVersion struct {
	ID string `json:"id"`
	// Scope and ScopeID select the override target: a provider id, a model id
	// (the registry row, i.e. one provider-model pair), a tenant id, or empty
	// for a global default.
	Scope   PricingScope `json:"scope"`
	ScopeID string       `json:"scope_id,omitempty"`
	// Currency is informational (ISO-4217, default USD). The engine computes in
	// the quoted currency's own units; no FX conversion is applied, because a
	// gateway silently converting currencies would produce invoices nobody can
	// reproduce from the price sheet.
	Currency                  string  `json:"currency"`
	InputCostPerMillion       float64 `json:"input_cost_per_million"`
	OutputCostPerMillion      float64 `json:"output_cost_per_million"`
	CachedInputCostPerMillion float64 `json:"cached_input_cost_per_million,omitempty"`
	// BaseFeeUSD is charged once per billable attempt, for providers with a
	// per-request overhead on top of token pricing.
	BaseFeeUSD float64 `json:"base_fee_usd,omitempty"`
	// EffectiveFrom is inclusive; EffectiveTo is exclusive and nil means the
	// version is current.
	EffectiveFrom time.Time  `json:"effective_from"`
	EffectiveTo   *time.Time `json:"effective_to,omitempty"`
	CreatedBy     string     `json:"created_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// EffectiveAt reports whether the version was the live sheet at instant t.
func (v *PricingVersion) EffectiveAt(t time.Time) bool {
	if v == nil {
		return false
	}
	if t.Before(v.EffectiveFrom) {
		return false
	}
	return v.EffectiveTo == nil || t.Before(*v.EffectiveTo)
}

// CostLineKind classifies one row of a cost breakdown.
type CostLineKind string

const (
	// CostLineInput prices prompt tokens served fresh at the full input rate.
	CostLineInput CostLineKind = "input"
	// CostLineCachedInput prices prompt tokens served from provider cache.
	CostLineCachedInput CostLineKind = "cached_input"
	// CostLineOutput prices completion tokens.
	CostLineOutput CostLineKind = "output"
	// CostLineBaseFee is a per-attempt overhead fee.
	CostLineBaseFee CostLineKind = "base_fee"
)

// CostLineItem is one auditable row of a computed cost: what was counted, at
// what unit price, and what it came to. Every amount is rounded to micro-USD
// and the breakdown total is the sum of the rounded lines, so amount columns
// always add up exactly to the reported total.
type CostLineItem struct {
	Kind CostLineKind `json:"kind"`
	// Label names the step for humans, e.g. "attempt 2 · output".
	Label string `json:"label"`
	// Quantity is tokens for token lines and attempt counts for fee lines.
	Quantity float64 `json:"quantity"`
	// UnitPriceUSD is USD per million tokens, or USD per attempt for fees.
	UnitPriceUSD float64 `json:"unit_price_usd"`
	AmountUSD    float64 `json:"amount_usd"`
}

// CostBreakdown is the full explainable account of one request's cost.
type CostBreakdown struct {
	Lines []CostLineItem `json:"lines"`
	// TotalUSD is the sum of the rounded line amounts, never a separately
	// rounded figure, so the invoice always foots.
	TotalUSD float64 `json:"total_usd"`
	Currency string  `json:"currency"`
	// PricingVersionID is the versioned sheet used, empty when the static
	// registry price applied. PricingSource names which scope won resolution.
	PricingVersionID string `json:"pricing_version_id,omitempty"`
	PricingSource    string `json:"pricing_source,omitempty"`
	// Estimated is true for a pre-execution projection rather than a final
	// account; estimates use full input rates because cache state is unknown.
	Estimated bool `json:"estimated"`
}

// Total returns the breakdown total as a Cost.
func (b CostBreakdown) Total() Cost { return Cost{USD: b.TotalUSD} }

// BudgetAlert is one fired threshold crossing, persisted so alerts are prompt
// and exactly-once per budget/threshold/period: the unique constraint, not
// application memory, is what prevents a second page for the same crossing.
type BudgetAlert struct {
	ID           string       `json:"id"`
	BudgetID     string       `json:"budget_id"`
	TenantID     string       `json:"tenant_id"`
	ThresholdUSD float64      `json:"threshold_usd"`
	SpentUSD     float64      `json:"spent_usd"`
	LimitUSD     float64      `json:"limit_usd"`
	Period       BudgetPeriod `json:"period"`
	// PeriodStart anchors the alert to one budget period.
	PeriodStart time.Time `json:"period_start"`
	FiredAt     time.Time `json:"fired_at"`
}

// AnomalySeverity grades a spend anomaly by how far observed spend overshoots
// the baseline.
type AnomalySeverity string

const (
	AnomalyInfo     AnomalySeverity = "info"
	AnomalyWarning  AnomalySeverity = "warning"
	AnomalyCritical AnomalySeverity = "critical"
)

// CostAnomaly is one flagged spend deviation, stored so the dashboard can show
// open anomalies without recomputing baselines on every page load.
type CostAnomaly struct {
	ID string `json:"id"`
	// Dimension is provider, model, tenant or endpoint; Key is the value.
	Dimension string `json:"dimension"`
	Key       string `json:"key"`
	// WindowFrom/To bound the anomalous day (anomalies are detected daily).
	WindowFrom  time.Time `json:"window_from"`
	WindowTo    time.Time `json:"window_to"`
	ObservedUSD float64   `json:"observed_usd"`
	ExpectedUSD float64   `json:"expected_usd"`
	// Ratio is observed/expected, the single number that defines severity.
	Ratio    float64         `json:"ratio"`
	Severity AnomalySeverity `json:"severity"`
	// ResolvedAt is set by an operator acknowledging the anomaly.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	DetectedAt time.Time  `json:"detected_at"`
}

// BudgetStatus is the evaluated state of one budget for the dashboard.
type BudgetStatus struct {
	BudgetID     string       `json:"budget_id"`
	TenantID     string       `json:"tenant_id"`
	Scope        string       `json:"scope"`
	Period       BudgetPeriod `json:"period"`
	LimitUSD     float64      `json:"limit_usd"`
	SpentUSD     float64      `json:"spent_usd"`
	RemainingUSD float64      `json:"remaining_usd"`
	// Utilization is spent/limit in [0, +inf); above 1 means overspent.
	Utilization float64 `json:"utilization"`
	// BurnRate is the fraction of the period's budget consumed per elapsed
	// fraction: 1.0 means exactly on pace, above 1 means burning too fast.
	BurnRate float64 `json:"burn_rate"`
	// ProjectedUSD is the end-of-period spend at the current pace.
	ProjectedUSD float64   `json:"projected_usd"`
	Exhausted    bool      `json:"exhausted"`
	Enforced     bool      `json:"enforced"`
	ResetAt      time.Time `json:"reset_at"`
}

// CostForecast projects spend forward from observed daily totals.
type CostForecast struct {
	// DailyAverageUSD is the trailing-7-day mean actually observed.
	DailyAverageUSD float64 `json:"daily_average_usd"`
	// TrendUSDPerDay is the least-squares slope: positive means spend is
	// climbing, negative means it is falling.
	TrendUSDPerDay float64 `json:"trend_usd_per_day"`
	// ProjectedMonthUSD is the end-of-month total at trend pace.
	ProjectedMonthUSD float64 `json:"projected_month_usd"`
	// ProjectedMonthLow/HighUSD bound the projection by one residual standard
	// deviation, so finance sees a range rather than a false point promise.
	ProjectedMonthLowUSD  float64   `json:"projected_month_low_usd"`
	ProjectedMonthHighUSD float64   `json:"projected_month_high_usd"`
	DaysObserved          int       `json:"days_observed"`
	GeneratedAt           time.Time `json:"generated_at"`
}

// CostOverview is the dashboard's cost landing response: totals plus the
// estimate-accuracy and savings figures that make spend explainable.
type CostOverview struct {
	WindowFrom time.Time `json:"window_from"`
	WindowTo   time.Time `json:"window_to"`
	Requests   int64     `json:"requests"`
	// ActualUSD is the sum of final computed costs; EstimatedUSD the sum of
	// routing-time projections. When estimates are good the two agree.
	ActualUSD    float64 `json:"actual_usd"`
	EstimatedUSD float64 `json:"estimated_usd"`
	// EstimateAccuracy is 1 - |actual-estimate|/actual over the window: 1.0 is
	// perfect foresight, falling toward 0 as projections drift.
	EstimateAccuracy float64 `json:"estimate_accuracy"`
	Tokens           int64   `json:"tokens"`
	// CostPerKiloTokenUSD is actual spend per 1k tokens, the unit rate finance
	// compares across windows.
	CostPerKiloTokenUSD float64 `json:"cost_per_kilo_token_usd"`
	// CacheSavingsUSD is spend avoided by served-from-cache responses;
	// RoutingSavingsUSD is spend avoided by shaping/optimization choices.
	CacheSavingsUSD   float64   `json:"cache_savings_usd"`
	RoutingSavingsUSD float64   `json:"routing_savings_usd"`
	GeneratedAt       time.Time `json:"generated_at"`
}

// CostDimensionRow is one row of a grouped spend report.
type CostDimensionRow struct {
	Key          string  `json:"key"`
	Requests     int64   `json:"requests"`
	Tokens       int64   `json:"tokens"`
	ActualUSD    float64 `json:"actual_usd"`
	EstimatedUSD float64 `json:"estimated_usd"`
	// Share is actual/window-total in [0,1], so rows read as "where spend goes".
	Share float64 `json:"share"`
}

// CostPoint is one bucket of a spend time series.
type CostPoint struct {
	Bucket       time.Time `json:"bucket"`
	Requests     int64     `json:"requests"`
	ActualUSD    float64   `json:"actual_usd"`
	EstimatedUSD float64   `json:"estimated_usd"`
}
