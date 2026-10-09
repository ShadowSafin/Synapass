package domain

import (
	"strings"
	"time"
)

// RequestContext is everything the routing engine, policy engine and providers
// need to know about one inbound request. It is built once by the API layer and
// passed by pointer; nothing downstream mutates it except the fields explicitly
// documented as mutable (Resolution, so a chain can record its own progress).
type RequestContext struct {
	// RequestID is Synapass's correlation id and is echoed to the client.
	RequestID RequestID `json:"request_id"`
	// TraceID and SpanID are the OpenTelemetry identifiers, so a request id in
	// a client bug report maps directly to a trace.
	TraceID string `json:"trace_id,omitempty"`
	SpanID  string `json:"span_id,omitempty"`
	// ReceivedAt is when the HTTP handler was entered.
	ReceivedAt time.Time `json:"received_at"`
	// StructuredOutput is the parsed response_format contract for this request.
	// Mode is FormatNone when the client did not ask for structured output, so
	// every consumer can check one field instead of re-parsing the wire format.
	StructuredOutput StructuredOutput `json:"-"`
	// Deadline is the absolute time by which a response must be produced. It is
	// the earlier of the policy's total timeout and any client-supplied
	// expectation, and it is what timeout derivation actually consults.
	Deadline time.Time `json:"deadline"`

	// Identity, resolved by the auth layer before routing runs.
	Tenant *Tenant `json:"-"`
	APIKey *APIKey `json:"-"`

	// RequestType is the API surface the request arrived on.
	RequestType RequestType `json:"request_type"`
	// RequestedModel is the client's unmodified model parameter.
	RequestedModel string `json:"requested_model"`
	// RequestedAlias is populated when the requested model is a gateway alias
	// rather than an upstream model name.
	RequestedAlias string `json:"requested_alias,omitempty"`

	// Stream is true when the client requested server-sent events.
	Stream bool `json:"stream"`
	// N is the number of completions requested.
	N int `json:"n"`
	// MaxOutputTokens is the effective completion allowance after applying the
	// client's request and the policy ceiling.
	MaxOutputTokens int `json:"max_output_tokens"`
	// RequestedOutputTokens is what the client actually asked for, 0 when it
	// said nothing. It is kept separately from MaxOutputTokens because the router
	// fills MaxOutputTokens in from the policy default when the client is
	// silent, which would otherwise look like an explicit request — and a
	// response shortened by that default must be reportable as such.
	RequestedOutputTokens int `json:"requested_output_tokens,omitempty"`
	// PromptTokens is the estimated prompt size.
	PromptTokens int `json:"prompt_tokens"`
	// Messages is retained read-only for prompt analysis and content matching.
	Messages []ChatMessage `json:"-"`

	// RequiredCapabilities is derived from the request, never trusted from it.
	RequiredCapabilities []Capability `json:"required_capabilities,omitempty"`
	// LatencyTargetMS is the client's or policy's soft latency objective.
	LatencyTargetMS int `json:"latency_target_ms,omitempty"`
	// CostCeilingUSD is the maximum the client will pay for this request. Zero
	// means "no client-imposed ceiling"; the policy ceiling still applies.
	CostCeilingUSD float64 `json:"cost_ceiling_usd,omitempty"`

	// Client provenance, used for abuse detection and audit records.
	ClientIP  string            `json:"client_ip,omitempty"`
	UserAgent string            `json:"user_agent,omitempty"`
	Headers   map[string]string `json:"-"`

	// Labels are operator metadata propagated into logs and traces.
	Labels map[string]string `json:"labels,omitempty"`

	// PolicyID records the policy that was applied, if the caller pinned one.
	PolicyID string `json:"policy_id,omitempty"`

	// Phase 2: intelligence context, populated before routing.
	Task TaskClassification `json:"task,omitempty"`
	// EndpointID is the admin-managed endpoint scope, when any.
	EndpointID string `json:"endpoint_id,omitempty"`
	// EndpointOverride is the scope's routing override, loaded by the API
	// layer before the engine resolves. The engine folds it into the policy
	// copy for this request, so scopes constrain routing without rewriting
	// stored policies.
	EndpointOverride *EndpointRoutingOverride `json:"-"`
	// Region pins provider geography, when required.
	Region string `json:"region,omitempty"`
	// DataSensitivity labels the request payload (e.g. "pii", "public").
	DataSensitivity []string `json:"data_sensitivity,omitempty"`
	// Batch is true for offline/batch traffic, false for interactive.
	Batch bool `json:"batch,omitempty"`
	// CacheBypass forces a cache miss (sensitive or operator override).
	CacheBypass bool `json:"cache_bypass,omitempty"`
	// RequestBytes is the wire size, for size guards.
	RequestBytes int `json:"request_bytes,omitempty"`

	// Resolution is the routing engine's output, stored on the context so the
	// rest of the pipeline can read the decision without a second lookup.
	Resolution *RouteDecision `json:"-"`
	// PolicyDecision is the Phase 2 policy engine verdict.
	PolicyDecision *PolicyDecision `json:"-"`
	// ShapePlan is the prompt shaping plan for this request.
	ShapePlan PromptShapePlan `json:"-"`
	// CacheDecision is the Phase 5 cache policy verdict for this request.
	CacheDecision *CacheDecision `json:"-"`
	// CacheLookupMS is the cache lookup latency for observability.
	CacheLookupMS float64 `json:"-"`
	// StreamTextRunes counts visible-text runes delivered downstream on a
	// streaming request. The chunk handler increments it; the error path
	// reads it to reconcile usage for work already performed when the
	// provider never reports final counts (cancel, timeout, disconnect).
	// Same-goroutine use only (handler and completion tail).
	StreamTextRunes int `json:"-"`
}

// Remaining returns the time left before the request deadline. A non-positive
// result means the budget is exhausted and the request must fail immediately
// rather than opening a doomed upstream connection.
func (c *RequestContext) Remaining(now time.Time) time.Duration {
	if c.Deadline.IsZero() {
		return 0
	}
	return c.Deadline.Sub(now)
}

// TenantID returns the tenant identifier, or an empty string when unauthenticated.
func (c *RequestContext) TenantID() string {
	if c.Tenant == nil {
		return ""
	}
	return c.Tenant.ID
}

// APIKeyID returns the key identifier, or an empty string when unauthenticated.
func (c *RequestContext) APIKeyID() string {
	if c.APIKey == nil {
		return ""
	}
	return c.APIKey.ID
}

// CapabilitySet returns the required capabilities as a set.
func (c *RequestContext) CapabilitySet() CapabilitySet {
	return NewCapabilitySet(c.RequiredCapabilities...)
}

// PolicyName returns the applied policy name when a decision exists.
func (c *RequestContext) PolicyName() string {
	if c.Resolution == nil {
		return ""
	}
	return c.Resolution.PolicyName
}

// Candidate is a route target that survived filtering, annotated with the
// reasons it survived and the signals used to rank it. Candidates are retained
// on the decision so an operator can answer "why not provider X?" from the log
// alone, without reproducing the request.
type Candidate struct {
	Target RouteTarget `json:"target"`
	// Group is the index of the policy target this candidate came from. It lets
	// the engine tell "the operator's first choice was unavailable" (group > 0)
	// from "the operator's first choice is degraded" without comparing targets.
	Group int `json:"group"`
	// Eligible is false when the candidate was considered but rejected.
	Eligible bool `json:"eligible"`
	// RejectionReason explains an ineligible candidate.
	RejectionReason string `json:"rejection_reason,omitempty"`
	// EstimatedCostUSD is the projected cost for this request.
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`
	// Score is the strategy-assigned rank score; lower is better for every
	// implemented strategy, which keeps comparisons uniform.
	Score float64 `json:"score,omitempty"`
	// Healthy reflects the health state used during ranking.
	Healthy bool `json:"healthy"`
	// Notes records human-readable ranking observations.
	Notes []string `json:"notes,omitempty"`
}

// RouteDecision is the routing engine's complete, auditable output.
type RouteDecision struct {
	RequestID  RequestID       `json:"request_id"`
	PolicyID   string          `json:"policy_id,omitempty"`
	PolicyName string          `json:"policy_name,omitempty"`
	Strategy   RoutingStrategy `json:"strategy"`
	// Chosen is the first target the engine will attempt.
	Chosen RouteTarget `json:"chosen"`
	// Chain is the full ordered attempt list including Chosen.
	Chain FallbackChain `json:"chain"`
	// Retry, Timeout and Fallback are the effective policies for this request.
	// They are copied onto the decision rather than re-read from the policy so
	// that an execution is fully described by the decision it executed, even if
	// the policy is edited while the request is in flight.
	Retry    RetryPolicy    `json:"retry"`
	Timeout  TimeoutPolicy  `json:"timeout"`
	Fallback FallbackPolicy `json:"fallback"`
	// EstimatedCost is the projected cost of the chosen target.
	EstimatedCost Cost `json:"estimated_cost"`
	// CostCeilingUSD is the ceiling actually enforced, after combining the
	// request and policy limits.
	CostCeilingUSD float64 `json:"cost_ceiling_usd,omitempty"`
	// Reason is a short human-readable justification.
	Reason string `json:"reason"`
	// Candidates is every target considered, in ranked order, including the
	// rejected ones.
	Candidates []Candidate `json:"candidates,omitempty"`
	// Degraded is true when the chosen target was not the policy's first choice.
	Degraded bool `json:"degraded"`
	// DecidedAt is when the decision was produced.
	DecidedAt time.Time `json:"decided_at"`
	// MatchedPolicyVersion is the policy revision, cited in audit events.
	MatchedPolicyVersion int `json:"matched_policy_version,omitempty"`
	// Phase 2: intelligence additions.
	Task      TaskClassification `json:"task,omitempty"`
	Shaping   PromptShapePlan    `json:"shaping,omitempty"`
	UseCache  bool               `json:"use_cache,omitempty"`
	CacheKind string             `json:"cache_kind,omitempty"`
	// TimeoutStrategy, RetryStrategy, PromptStrategy describe derived plans.
	TimeoutStrategy string `json:"timeout_strategy,omitempty"`
	RetryStrategy   string `json:"retry_strategy,omitempty"`
	PromptStrategy  string `json:"prompt_strategy,omitempty"`
	// ScoreNotes explains score-influenced ordering.
	ScoreNotes []string `json:"score_notes,omitempty"`
	// Rejected explains policy-level rejections carried into the decision.
	Rejected []RejectedTarget `json:"rejected,omitempty"`
	// CostEstimateBefore/After capture shaping or model-substitution effects.
	CostBeforeUSD float64 `json:"cost_before_usd,omitempty"`
	CostAfterUSD  float64 `json:"cost_after_usd,omitempty"`
}

// FallbackChain is the ordered list of targets a request may attempt. It is a
// distinct type rather than a bare slice because the invariants matter: the
// chain always has at least one target, iteration is always in order, and the
// attempt budget is carried alongside so the executor cannot exceed it.
type FallbackChain struct {
	// Targets are ordered by preference; Targets[0] is the primary.
	Targets []RouteTarget `json:"targets"`
	// Skipped records targets that were excluded from the chain and why. Keeping
	// these on the decision is what makes "why didn't it fail over to X?"
	// answerable after the fact.
	Skipped []SkippedTarget `json:"skipped,omitempty"`
	// MaxAttempts caps how many targets the executor may try. It is always at
	// least 1 and never more than len(Targets).
	MaxAttempts int `json:"max_attempts"`
}

// SkippedTarget is a target excluded from the chain with a reason.
type SkippedTarget struct {
	Target RouteTarget `json:"target"`
	Reason string      `json:"reason"`
}

// Primary returns the first target in the chain.
func (c FallbackChain) Primary() RouteTarget {
	if len(c.Targets) == 0 {
		return RouteTarget{}
	}
	return c.Targets[0]
}

// Empty reports whether the chain has no targets.
func (c FallbackChain) Empty() bool { return len(c.Targets) == 0 }

// Length returns the number of targets in the chain.
func (c FallbackChain) Length() int { return len(c.Targets) }

// Limit returns the number of targets the executor may actually attempt.
func (c FallbackChain) Limit() int {
	n := c.MaxAttempts
	if n <= 0 || n > len(c.Targets) {
		n = len(c.Targets)
	}
	return n
}

// Attempts returns the targets the executor may try, in order.
func (c FallbackChain) Attempts() []RouteTarget {
	limit := c.Limit()
	if limit >= len(c.Targets) {
		return c.Targets
	}
	return c.Targets[:limit]
}

// Truncate returns a copy of the chain limited to n targets, preserving the
// attempt cap. Truncation is used when a retry budget or the request deadline
// makes later targets unreachable.
func (c FallbackChain) Truncate(n int) FallbackChain {
	if n < 0 {
		n = 0
	}
	out := c
	if n < len(out.Targets) {
		out.Targets = append([]RouteTarget(nil), out.Targets[:n]...)
	}
	if out.MaxAttempts > len(out.Targets) {
		out.MaxAttempts = len(out.Targets)
	}
	if out.MaxAttempts < 1 && len(out.Targets) > 0 {
		out.MaxAttempts = 1
	}
	return out
}

// Explain renders the decision as a single log-friendly line. Structured logs
// carry the full struct; this string exists for human debugging and for the
// dashboard's compact table view.
func (d *RouteDecision) Explain() string {
	if d == nil {
		return "no decision"
	}
	var b strings.Builder
	b.WriteString("policy=")
	if d.PolicyName != "" {
		b.WriteString(d.PolicyName)
	} else {
		b.WriteString("(default)")
	}
	b.WriteString(" strategy=")
	b.WriteString(string(d.Strategy))
	b.WriteString(" chosen=")
	b.WriteString(d.Chosen.Ref().String())
	b.WriteString(" chain=")
	for i, t := range d.Chain.Attempts() {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(t.Ref().String())
	}
	b.WriteString(" est_cost=")
	b.WriteString(d.EstimatedCost.String())
	return b.String()
}
