package domain

import "time"

// ---------------------------------------------------------------------------
// Phase 2: task classification
// ---------------------------------------------------------------------------

// TaskType is the inferred workload class of a request. Classification is
// deterministic and rules-first: the same request always yields the same task,
// which keeps routing reproducible and testable.
type TaskType string

const (
	TaskChat             TaskType = "chat"
	TaskCoding           TaskType = "coding"
	TaskSummarization    TaskType = "summarization"
	TaskExtraction       TaskType = "extraction"
	TaskReasoning        TaskType = "reasoning"
	TaskTranslation      TaskType = "translation"
	TaskToolUse          TaskType = "tool-use"
	TaskStructuredOutput TaskType = "structured_output"
	TaskLongContext      TaskType = "long_context"
	TaskInteractive      TaskType = "high_priority_interactive"
	TaskBatch            TaskType = "batch_offline"
	TaskUnknown          TaskType = "unknown"
)

// Valid reports whether the task type is known.
func (t TaskType) Valid() bool {
	switch t {
	case TaskChat, TaskCoding, TaskSummarization, TaskExtraction, TaskReasoning,
		TaskTranslation, TaskToolUse, TaskStructuredOutput, TaskLongContext,
		TaskInteractive, TaskBatch, TaskUnknown:
		return true
	default:
		return false
	}
}

// Interactive reports whether the task is latency-sensitive.
func (t TaskType) Interactive() bool {
	switch t {
	case TaskChat, TaskInteractive, TaskToolUse, TaskStructuredOutput:
		return true
	default:
		return false
	}
}

// TaskClassification is the classifier output attached to a request.
type TaskClassification struct {
	// Task is the primary inferred class.
	Task TaskType `json:"task"`
	// Secondary lists additional classes that also matched.
	Secondary []TaskType `json:"secondary,omitempty"`
	// Confidence is in [0,1]; rules-first classification reports coarse
	// buckets (1.0 exact signal, 0.8 strong, 0.6 heuristic) rather than a
	// calibrated probability.
	Confidence float64 `json:"confidence"`
	// Signals names the rules that fired, for explainability.
	Signals []string `json:"signals,omitempty"`
	// PromptTokens and MessageCount are the inputs the decision used.
	PromptTokens int `json:"prompt_tokens"`
	MessageCount int `json:"message_count"`
	// ClassifiedAt is when the classification ran.
	ClassifiedAt time.Time `json:"classified_at"`
	// Source is "rules" for the Go classifier or "python" for async analysis.
	Source string `json:"source,omitempty"`
}

// PrimaryOrDefault returns the task or chat when empty.
func (c TaskClassification) PrimaryOrDefault() TaskType {
	if c.Task == "" {
		return TaskChat
	}
	return c.Task
}

// Explain renders the classification for logs.
func (c TaskClassification) Explain() string {
	if len(c.Signals) == 0 {
		return string(c.PrimaryOrDefault())
	}
	out := string(c.PrimaryOrDefault()) + " ("
	for i, s := range c.Signals {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out + ")"
}

// ---------------------------------------------------------------------------
// Phase 2: policy decision
// ---------------------------------------------------------------------------

// PolicyDecision is the policy engine output consumed directly by the router.
// It records both the verdict and the reasons so the request is explainable.
type PolicyDecision struct {
	RequestID  RequestID `json:"request_id"`
	PolicyID   string    `json:"policy_id,omitempty"`
	PolicyName string    `json:"policy_name,omitempty"`
	// Allowed is false when the request is denied before routing.
	Allowed bool `json:"allowed"`
	// DenyReason is the machine-readable code; DenyMessage is human-readable.
	DenyReason  string `json:"deny_reason,omitempty"`
	DenyMessage string `json:"deny_message,omitempty"`
	// Warnings are non-blocking observations (e.g. near budget).
	Warnings []string `json:"warnings,omitempty"`
	// MatchedRules names the rules that contributed to the decision.
	MatchedRules []string `json:"matched_rules,omitempty"`
	// RejectedTargets explains candidates removed by policy (allow/deny lists,
	// region, sensitivity, cost/latency ceilings).
	RejectedTargets []RejectedTarget `json:"rejected_targets,omitempty"`
	// EffectiveLimits are the limits actually enforced for this request.
	EffectiveLimits PolicyLimits `json:"effective_limits,omitempty"`
	// UseCache, Shaping, Task flow into downstream stages.
	UseCache bool               `json:"use_cache"`
	Shaping  PromptShapePlan    `json:"shaping,omitempty"`
	Task     TaskClassification `json:"task,omitempty"`
	// EndpointID is the resolved endpoint/route scope, when any.
	EndpointID string `json:"endpoint_id,omitempty"`
	// DecidedAt is when the decision was produced.
	DecidedAt time.Time `json:"decided_at"`
	// PolicyVersion cites the exact rule revision.
	PolicyVersion int `json:"policy_version,omitempty"`
}

// RejectedTarget explains one policy-level rejection.
type RejectedTarget struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Reason   string `json:"reason"`
	Rule     string `json:"rule,omitempty"`
}

// Allowed helper for call sites.
func (d *PolicyDecision) IsAllowed() bool { return d == nil || d.Allowed }

// Explain renders the decision as a log-friendly line.
func (d *PolicyDecision) Explain() string {
	if d == nil {
		return "policy: allow (no decision)"
	}
	if !d.Allowed {
		return "policy: deny (" + d.DenyReason + "): " + d.DenyMessage
	}
	s := "policy: allow"
	if d.PolicyName != "" {
		s += " via " + d.PolicyName
	}
	if len(d.Warnings) > 0 {
		s += " warnings=" + itoaStr(len(d.Warnings))
	}
	return s
}

func itoaStr(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// PolicyRule is a durable fine-grained rule row. Stored rules are compiled
// into the in-memory RoutingPolicy match+limits at load time; this type is the
// API/storage shape for CRUD.
type PolicyRule struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id,omitempty"`
	APIKeyID    string `json:"api_key_id,omitempty"`
	EndpointID  string `json:"endpoint_id,omitempty"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Priority    int    `json:"priority"`
	Enabled     bool   `json:"enabled"`
	// Scope selects the level: tenant | key | endpoint | global.
	Scope string `json:"scope"`
	// Allow/Deny lists for models and providers (glob patterns allowed).
	AllowModels    []string `json:"allow_models,omitempty"`
	DenyModels     []string `json:"deny_models,omitempty"`
	AllowProviders []string `json:"allow_providers,omitempty"`
	DenyProviders  []string `json:"deny_providers,omitempty"`
	// Numeric guards.
	MaxCostPerRequestUSD float64 `json:"max_cost_per_request_usd,omitempty"`
	MaxLatencyMS         int     `json:"max_latency_ms,omitempty"`
	MaxPromptTokens      int     `json:"max_prompt_tokens,omitempty"`
	MaxRequestBytes      int     `json:"max_request_bytes,omitempty"`
	// Region and sensitivity constraints.
	AllowedRegions   []string `json:"allowed_regions,omitempty"`
	DeniedRegions    []string `json:"denied_regions,omitempty"`
	DataSensitivity  string   `json:"data_sensitivity,omitempty"`
	AllowedSensitive []string `json:"allowed_sensitive,omitempty"`
	// Batch vs interactive.
	BatchOnly       bool `json:"batch_only,omitempty"`
	InteractiveOnly bool `json:"interactive_only,omitempty"`
	// Effect is allow | deny | constrain.
	Effect    string    `json:"effect"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Phase 2: prompt shaping
// ---------------------------------------------------------------------------

// PromptShapeStep is one transformation applied to the prompt.
type PromptShapeStep struct {
	Name    string `json:"name"`
	Detail  string `json:"detail,omitempty"`
	Applied bool   `json:"applied"`
	// TokensBefore/After measure the step effect, when known.
	TokensBefore int `json:"tokens_before,omitempty"`
	TokensAfter  int `json:"tokens_after,omitempty"`
}

// PromptShapePlan is the shaping decision for a request.
type PromptShapePlan struct {
	NormalizeSystem    bool              `json:"normalize_system,omitempty"`
	Compress           bool              `json:"compress,omitempty"`
	TrimContext        bool              `json:"trim_context,omitempty"`
	SummarizeHistory   bool              `json:"summarize_history,omitempty"`
	FormatStructured   bool              `json:"format_structured,omitempty"`
	ToolPrompting      bool              `json:"tool_prompting,omitempty"`
	GuardrailInject    bool              `json:"guardrail_inject,omitempty"`
	ProviderAdapt      bool              `json:"provider_adapt,omitempty"`
	MaxHistoryMessages int               `json:"max_history_messages,omitempty"`
	MaxPromptTokens    int               `json:"max_prompt_tokens,omitempty"`
	SystemPrefix       string            `json:"system_prefix,omitempty"`
	SystemSuffix       string            `json:"system_suffix,omitempty"`
	Guardrails         []string          `json:"guardrails,omitempty"`
	Steps              []PromptShapeStep `json:"steps,omitempty"`
}

// PromptShape is the durable record of what shaping did.
type PromptShape struct {
	ID        string          `json:"id"`
	RequestID RequestID       `json:"request_id"`
	TenantID  string          `json:"tenant_id,omitempty"`
	Plan      PromptShapePlan `json:"plan"`
	TokensIn  int             `json:"tokens_in"`
	TokensOut int             `json:"tokens_out"`
	Truncated int             `json:"truncated_messages,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

// Explain renders the plan.
func (p PromptShapePlan) Explain() string {
	out := ""
	appendFlag := func(name string, on bool) {
		if on {
			if out != "" {
				out += ","
			}
			out += name
		}
	}
	appendFlag("normalize", p.NormalizeSystem)
	appendFlag("compress", p.Compress)
	appendFlag("trim", p.TrimContext)
	appendFlag("summarize", p.SummarizeHistory)
	appendFlag("structured", p.FormatStructured)
	appendFlag("tools", p.ToolPrompting)
	appendFlag("guardrails", p.GuardrailInject)
	appendFlag("adapt", p.ProviderAdapt)
	if out == "" {
		return "shaping: none"
	}
	return "shaping: " + out
}

// ---------------------------------------------------------------------------
// Phase 2: caching
// ---------------------------------------------------------------------------

// CacheKind classifies a cache entry.
type CacheKind string

const (
	CacheExact    CacheKind = "exact"
	CachePrefix   CacheKind = "prefix"
	CacheSemantic CacheKind = "semantic"
)

// CacheEntry is the durable metadata row; the body lives in Redis.
type CacheEntry struct {
	ID         string    `json:"id"`
	TenantID   string    `json:"tenant_id"`
	Kind       CacheKind `json:"kind"`
	CacheKey   string    `json:"cache_key"`
	Model      string    `json:"model,omitempty"`
	PromptHash string    `json:"prompt_hash"`
	// EmbeddingHash is set for semantic entries.
	EmbeddingHash string `json:"embedding_hash,omitempty"`
	// Similarity of the lookup that produced a hit, for semantic entries.
	Similarity float64 `json:"similarity,omitempty"`
	// Sensitive entries are never cached; this flag records the bypass reason.
	Sensitive bool `json:"sensitive,omitempty"`
	// HitCount tracks reuse.
	HitCount  int       `json:"hit_count"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Phase 5 additions: serving context and dashboard preview.
	Provider      string `json:"provider,omitempty"`
	PolicyID      string `json:"policy_id,omitempty"`
	EndpointID    string `json:"endpoint_id,omitempty"`
	APIKeyID      string `json:"api_key_id,omitempty"`
	PromptPreview string `json:"prompt_preview,omitempty"`
	ReuseCount    int64  `json:"reuse_count,omitempty"`
}

// CacheStats is the admin cache overview.
//
// Phase 5 extends the Phase 2 counters without renaming them: existing
// dashboards keep working, and the new fields explain *why* misses happen
// and how much work hits saved.
type CacheStats struct {
	ExactHits    int64   `json:"exact_hits"`
	ExactMisses  int64   `json:"exact_misses"`
	PrefixHits   int64   `json:"prefix_hits"`
	SemanticHits int64   `json:"semantic_hits"`
	Bypasses     int64   `json:"bypasses"`
	HitRate      float64 `json:"hit_rate"`
	Entries      int64   `json:"entries,omitempty"`
	// Phase 5 additions.
	Misses         int64            `json:"misses,omitempty"`
	BypassByReason map[string]int64 `json:"bypass_by_reason,omitempty"`
	Invalidations  int64            `json:"invalidations,omitempty"`
	ReuseCount     int64            `json:"reuse_count,omitempty"`
	LatencySavedMS int64            `json:"latency_saved_ms,omitempty"`
	// CostSavedUSD accumulates the stored serving cost of reused answers:
	// each hit adds the original request's billed cost, which is the
	// standing delta the cache avoided re-spending.
	CostSavedUSD   float64          `json:"cost_saved_usd,omitempty"`
	LookupMSAvg    float64          `json:"lookup_ms_avg,omitempty"`
	TopPrompts     []CacheTopPrompt `json:"top_prompts,omitempty"`
}

// CacheTopPrompt is one frequently-served prompt for the dashboard.
type CacheTopPrompt struct {
	PromptHash    string `json:"prompt_hash"`
	PromptPreview string `json:"prompt_preview,omitempty"`
	Model         string `json:"model,omitempty"`
	Hits          int64  `json:"hits"`
}

// CacheLookupResult is the outcome of a cache check.
type CacheLookupResult struct {
	Hit          bool      `json:"hit"`
	Kind         CacheKind `json:"kind,omitempty"`
	Body         []byte    `json:"-"`
	Key          string    `json:"key,omitempty"`
	Similarity   float64   `json:"similarity,omitempty"`
	BypassReason string    `json:"bypass_reason,omitempty"`
	// Phase 5 trace fields.
	LookupMS       float64      `json:"lookup_ms,omitempty"`
	ReuseCount     int64        `json:"reuse_count,omitempty"`
	LatencySavedMS int64        `json:"latency_saved_ms,omitempty"`
	Meta           *CacheHitMeta `json:"meta,omitempty"`
	Trace          *CacheTrace   `json:"trace,omitempty"`
}

// ---------------------------------------------------------------------------
// Phase 2: scoring
// ---------------------------------------------------------------------------

// ProviderScore is the explainable rollup for one provider.
type ProviderScore struct {
	ID             string             `json:"id"`
	ProviderID     string             `json:"provider_id"`
	ProviderName   string             `json:"provider_name,omitempty"`
	Window         string             `json:"window"`
	Requests       int                `json:"requests"`
	SuccessRate    float64            `json:"success_rate"`
	TimeoutRate    float64            `json:"timeout_rate"`
	RefusalRate    float64            `json:"refusal_rate"`
	AvgLatencyMS   float64            `json:"avg_latency_ms"`
	CostPerSuccess float64            `json:"cost_per_success"`
	FallbackRate   float64            `json:"fallback_rate"`
	FeedbackScore  float64            `json:"feedback_score,omitempty"`
	ByTask         map[string]float64 `json:"by_task,omitempty"`
	// Score is the blended [0,1] rank signal; higher is better.
	Score       float64   `json:"score"`
	Explanation string    `json:"explanation,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// ModelScore is the per-model rollup.
type ModelScore struct {
	ID             string    `json:"id"`
	ProviderID     string    `json:"provider_id,omitempty"`
	ProviderName   string    `json:"provider_name,omitempty"`
	Model          string    `json:"model"`
	Window         string    `json:"window"`
	Requests       int       `json:"requests"`
	SuccessRate    float64   `json:"success_rate"`
	AvgLatencyMS   float64   `json:"avg_latency_ms"`
	CostPerSuccess float64   `json:"cost_per_success"`
	Score          float64   `json:"score"`
	Explanation    string    `json:"explanation,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// ---------------------------------------------------------------------------
// Phase 2: eval and replay
// ---------------------------------------------------------------------------

// ReplayJob is a request to re-execute captured traffic offline.
type ReplayJob struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id,omitempty"`
	Name        string     `json:"name,omitempty"`
	Status      string     `json:"status"`
	RequestIDs  []string   `json:"request_ids,omitempty"`
	Dataset     string     `json:"dataset,omitempty"`
	Providers   []string   `json:"providers,omitempty"`
	Models      []string   `json:"models,omitempty"`
	MaxRequests int        `json:"max_requests,omitempty"`
	CreatedBy   string     `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Error       string     `json:"error,omitempty"`
	Progress    int        `json:"progress,omitempty"`
	Total       int        `json:"total,omitempty"`
}

// RequestPayload is the captured prompt behind one request id, retained so a
// pasted request id can be re-executed offline. It stores the normalized
// messages (roles preserved) rather than the wire body, which is what the
// provider adapters consume. Payloads are diagnostic, not billing: they carry
// no cost fields and are pruned with the request logs.
type RequestPayload struct {
	RequestID        string        `json:"request_id"`
	TenantID         string        `json:"tenant_id,omitempty"`
	Model            string        `json:"model,omitempty"`
	Messages         []ChatMessage `json:"messages"`
	MaxOutputTokens  int           `json:"max_output_tokens,omitempty"`
	CreatedAt        time.Time     `json:"created_at"`
}

// EvaluationRun groups one eval execution.
type EvaluationRun struct {
	ID          string     `json:"id"`
	TenantID    string     `json:"tenant_id,omitempty"`
	ReplayJobID string     `json:"replay_job_id,omitempty"`
	Dataset     string     `json:"dataset,omitempty"`
	Status      string     `json:"status"`
	CreatedBy   string     `json:"created_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

// EvaluationResult is one scored comparison.
type EvaluationResult struct {
	ID            string             `json:"id"`
	EvaluationID  string             `json:"evaluation_id"`
	RequestID     string             `json:"request_id,omitempty"`
	Provider      string             `json:"provider"`
	Model         string             `json:"model"`
	Score         float64            `json:"score"`
	LatencyMS     int64              `json:"latency_ms"`
	CostUSD       float64            `json:"cost_usd"`
	OutputPreview string             `json:"output_preview,omitempty"`
	IsRegression  bool               `json:"is_regression,omitempty"`
	Metrics       map[string]float64 `json:"metrics,omitempty"`
	CreatedAt     time.Time          `json:"created_at"`
}

// ---------------------------------------------------------------------------
// Phase 2: guardrails / overrides / feedback / analytics
// ---------------------------------------------------------------------------

// AuditOverride records an operator override (kill switch, budget change, ...).
type AuditOverride struct {
	ID        string     `json:"id"`
	TenantID  string     `json:"tenant_id,omitempty"`
	Kind      string     `json:"kind"`
	Target    string     `json:"target,omitempty"`
	Enabled   bool       `json:"enabled"`
	Reason    string     `json:"reason,omitempty"`
	Actor     string     `json:"actor,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

// CircuitBreakerState is the durable breaker view.
type CircuitBreakerState struct {
	ProviderID string     `json:"provider_id"`
	State      string     `json:"state"`
	Failures   int        `json:"failures"`
	OpenedAt   *time.Time `json:"opened_at,omitempty"`
	UpdatedAt  time.Time  `json:"updated_at"`
	Reason     string     `json:"reason,omitempty"`
	Overridden bool       `json:"overridden,omitempty"`
	OverrideBy string     `json:"override_by,omitempty"`
}

// FeedbackEvent is explicit user feedback on a request.
type FeedbackEvent struct {
	ID        string    `json:"id"`
	RequestID RequestID `json:"request_id"`
	TenantID  string    `json:"tenant_id,omitempty"`
	// Score is -1 | 0 | 1 (down | neutral | up) or a 1..5 rating.
	Score     float64   `json:"score"`
	Comment   string    `json:"comment,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Endpoint is an admin-managed route scope (e.g. "mobile-chat").
type Endpoint struct {
	ID          string `json:"id"`
	TenantID    string `json:"tenant_id,omitempty"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// RoutingOverride pins or biases routing for this endpoint.
	RoutingOverride *EndpointRoutingOverride `json:"routing_override,omitempty"`
	Enabled         bool                     `json:"enabled"`
	CreatedAt       time.Time                `json:"created_at"`
	UpdatedAt       time.Time                `json:"updated_at"`
}

// EndpointRoutingOverride biases routing for one endpoint.
type EndpointRoutingOverride struct {
	Strategy           RoutingStrategy `json:"strategy,omitempty"`
	PreferredModels    []string        `json:"preferred_models,omitempty"`
	PreferredProviders []string        `json:"preferred_providers,omitempty"`
	MaxCostUSD         float64         `json:"max_cost_usd,omitempty"`
	LatencyTargetMS    int             `json:"latency_target_ms,omitempty"`
	UseCache           *bool           `json:"use_cache,omitempty"`
	ForceModel         string          `json:"force_model,omitempty"`
	BlockFallback      bool            `json:"block_fallback,omitempty"`
}

// RoutingExplanation is the admin explain response.
type RoutingExplanation struct {
	RequestID        string             `json:"request_id,omitempty"`
	Task             TaskClassification `json:"task"`
	Policy           *PolicyDecision    `json:"policy,omitempty"`
	Decision         *RouteDecision     `json:"decision,omitempty"`
	Shaping          PromptShapePlan    `json:"shaping,omitempty"`
	Cache            *CacheLookupResult `json:"cache,omitempty"`
	ScoresConsidered []ProviderScore    `json:"scores_considered,omitempty"`
	Rejected         []RejectedTarget   `json:"rejected,omitempty"`
	Reason           string             `json:"reason"`
}
