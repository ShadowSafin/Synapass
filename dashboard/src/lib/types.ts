/**
 * TypeScript mirrors of the gateway's admin API payloads.
 *
 * These are hand-written rather than generated. The set is small and changes
 * rarely, and a hand-written type can carry the doc comment that explains what a
 * field means -- which is the difference between a rendering decision and a
 * guess. Every field name matches the Go struct's JSON tag in internal/domain.
 */

export type UsageOutcome = 'success' | 'fallback' | 'error' | 'rejected' | 'canceled';

export type HealthState = 'healthy' | 'degraded' | 'unhealthy' | 'unknown';

export type Status = 'active' | 'disabled' | 'degraded' | 'pending';

export type ProviderKind = 'openai' | 'anthropic' | 'ollama' | 'vllm' | 'openai_compatible';

export type ProviderStatus = 'active' | 'degraded' | 'disabled' | 'pending';

export type ProviderEnvironment = 'production' | 'internal' | 'external' | 'test';

export type RoutingStrategy =
  | 'priority'
  | 'weighted'
  | 'lowest_cost'
  | 'lowest_latency'
  | 'highest_quality';

export interface TimeRange {
  from: string;
  to: string;
  interval: string;
}

export interface TimeBucket {
  start: string;
  end: string;
  interval: string;
  requests: number;
  successes: number;
  errors: number;
  rejections: number;
  fallbacks: number;
  cache_hits: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  cost_usd: number;
  latency_p50_ms: number;
  latency_p90_ms: number;
  latency_p95_ms: number;
  latency_p99_ms: number;
}

export interface UsageSummary {
  requests: number;
  successes: number;
  errors: number;
  rejections: number;
  canceled: number;
  fallbacks: number;
  error_rate: number;
  fallback_rate: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  total_cost_usd: number;
  avg_cost_usd: number;
  avg_latency_ms: number;
  latency_p50_ms: number;
  latency_p90_ms: number;
  latency_p95_ms: number;
  latency_p99_ms: number;
  unique_tenants: number;
  unique_keys: number;
}

/**
 * One aggregate for a single value of a grouping dimension.
 *
 * The rates and per-unit figures are computed server-side from the same counts,
 * so the UI never divides one aggregate by another.
 */
export interface DimensionRow {
  key: string;
  requests: number;
  successes: number;
  errors: number;
  rejections: number;
  fallbacks: number;
  cache_hits: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  cached_prompt_tokens: number;
  cost_usd: number;
  avg_latency_ms: number;
  latency_p50_ms: number;
  latency_p90_ms: number;
  latency_p95_ms: number;
  latency_p99_ms: number;
  success_rate: number;
  error_rate: number;
  fallback_rate: number;
  cache_hit_rate: number;
  cost_per_success_usd: number;
  tokens_per_request: number;
}

/** Whether caching is helping, with the two populations it is derived from. */
export interface CacheReport {
  hits: number;
  misses: number;
  hit_rate: number;
  hit_avg_latency_ms: number;
  miss_avg_latency_ms: number;
  latency_saved_per_hit_ms: number;
  latency_saved_total_ms: number;
  cost_saved_usd: number;
  cached_prompt_tokens: number;
  prompt_cache_share: number;
  active: boolean;
}

/** One routing strategy's share of traffic. */
export interface RoutingStrategyRow {
  strategy: string;
  requests: number;
  fallbacks: number;
  errors: number;
  avg_attempts: number;
  fallback_rate: number;
}

export interface RoutingReport {
  strategies: RoutingStrategyRow[] | null;
  requests: number;
  fallbacks: number;
  errors: number;
  fallback_rate: number;
  avg_attempts: number;
  /** Share of requests where the router did not stay with its first choice. */
  provider_switch_rate: number;
}

/** One metric across the current and previous windows. */
export interface MetricDelta {
  metric: string;
  current: number;
  previous: number;
  change: number;
  /** Undefined-as-zero when the previous window had no activity. */
  change_ratio: number;
  higher_is_worse: boolean;
  unit: 'count' | 'ratio' | 'ms' | 'usd';
}

/** The composed analytics payload: one window, every breakdown. */
export interface AnalyticsReport {
  window: TimeRange;
  interval: string;
  summary: UsageSummary;
  previous_summary: UsageSummary;
  buckets: TimeBucket[] | null;
  comparison: MetricDelta[] | null;
  providers: DimensionRow[] | null;
  models: DimensionRow[] | null;
  tenants: DimensionRow[] | null;
  policies: DimensionRow[] | null;
  request_types: DimensionRow[] | null;
  outcomes: DimensionRow[] | null;
  errors: DimensionRow[] | null;
  cache: CacheReport;
  routing: RoutingReport;
  generated_at: string;
}

export interface ProviderBreakdownRow {
  provider: string;
  requests: number;
  errors: number;
  error_rate: number;
  fallbacks: number;
  total_tokens: number;
  cost_usd: number;
  avg_latency_ms: number;
  latency_p95_ms: number;
}

export interface ModelBreakdownRow {
  model: string;
  provider?: string;
  requests: number;
  errors: number;
  total_tokens: number;
  cost_usd: number;
  avg_latency_ms: number;
}

export interface ProviderHealth {
  provider_id: string;
  provider_name?: string;
  state: HealthState;
  checked_at: string;
  latency_ms: number;
  success_rate: number;
  consecutive_failures: number;
  error_rate: number;
  message?: string;
  source?: string;
  probe_latency_ms?: number;
}

export interface Overview {
  window: TimeRange;
  summary: UsageSummary;
  previous_summary: UsageSummary;
  series: TimeBucket[] | null;
  providers: ProviderBreakdownRow[] | null;
  models: ModelBreakdownRow[] | null;
  provider_health: ProviderHealth[] | null;
}

export interface ProviderSummary {
  id: string;
  name: string;
  kind: string;
  base_url: string;
  status: Status;
  capabilities: string[] | null;
  priority: number;
  weight: number;
  region: string;
  model_count: number;
  health?: ProviderHealth;
  adapter_ready: boolean;
  labels?: Record<string, string>;
  notes?: string;
  environment?: string;
  managed_by?: string;
  has_credential?: boolean;
}

/** Full provider detail as returned by POST /providers and GET /providers/{id}. */
export interface Provider extends ProviderSummary {
  api_key_env?: string;
  auth_style?: string;
  header_name?: string;
  headers?: Record<string, string>;
  organization?: string;
  project?: string;
  timeout_ms?: number;
  max_concurrency?: number;
  credential?: CredentialMeta | null;
}

/** Credential metadata. Never contains the secret itself. */
export interface CredentialMeta {
  name?: string;
  key_version?: number;
  created_at?: string;
}

export interface ProviderInput {
  name: string;
  kind: ProviderKind | string;
  base_url: string;
  api_key_env?: string;
  auth_style?: string;
  header_name?: string;
  headers?: Record<string, string>;
  organization?: string;
  project?: string;
  capabilities?: string[];
  status?: string;
  weight?: number;
  priority?: number;
  timeout_ms?: number;
  max_concurrency?: number;
  region?: string;
  notes?: string;
  environment?: string;
  labels?: Record<string, string>;
  /** Write-only initial credential; never read back. */
  credential_secret?: string;
  credential_name?: string;
}

export interface ProviderTestResult {
  kind: string;
  success: boolean;
  latency_ms?: number;
  message?: string;
  detail?: unknown;
  created_at?: string;
}

export interface ProviderTestResponse {
  provider_id: string;
  success: boolean;
  results: ProviderTestResult[] | null;
}

export interface ModelInput {
  provider_id: string;
  name: string;
  aliases?: string[];
  display_name?: string;
  context_window?: number;
  max_output_tokens?: number;
  capabilities?: string[];
  input_cost_per_million?: number;
  output_cost_per_million?: number;
  cached_input_cost_per_million?: number;
  status?: string;
  quality_tier?: number;
  rate_limit_rpm?: number;
  rate_limit_tpm?: number;
  priority?: number;
  environment?: string;
  metadata?: Record<string, string>;
}

export interface TenantInput {
  slug: string;
  name: string;
  status?: string;
  plan?: string;
  labels?: Record<string, string>;
  default_routing_policy_id?: string;
}

export interface KeyUpdateInput {
  name?: string;
  scopes?: string[];
  /** RFC3339 timestamp string, or null to clear the expiry. */
  expires_at?: string | null;
  routing_policy_id?: string | null;
}

export interface OverrideInput {
  kind: string;
  target?: string;
  tenant_id?: string;
  enabled: boolean;
  reason?: string;
  expires_at?: string;
}

export interface Override {
  id: string;
  kind: string;
  target?: string;
  tenant_id?: string;
  enabled: boolean;
  reason?: string;
  actor?: string;
  expires_at?: string;
  created_at?: string;
}

export interface PolicyInput {
  name: string;
  description?: string;
  priority?: number;
  enabled?: boolean;
  strategy?: RoutingStrategy | string;
  match?: RoutingPolicy['match'];
  targets?: RouteTarget[] | null;
  fallback?: RoutingPolicy['fallback'];
  retry?: RoutingPolicy['retry'];
  timeout?: RoutingPolicy['timeout'];
  limits?: RoutingPolicy['limits'];
}

export interface Model {
  id: string;
  provider_id: string;
  provider_name?: string;
  name: string;
  aliases?: string[];
  display_name?: string;
  version?: string;
  context_window: number;
  max_output_tokens: number;
  capabilities: string[] | null;
  input_cost_per_million: number;
  output_cost_per_million: number;
  cached_input_cost_per_million?: number;
  status: string;
  quality_tier?: number;
  rate_limit_rpm?: number;
  rate_limit_tpm?: number;
  priority?: number;
  environment?: string;
  deprecated_at?: string;
  metadata?: Record<string, string>;
  created_at: string;
  updated_at: string;
}

export interface RouteTarget {
  provider_id?: string;
  provider_name?: string;
  model: string;
  alias?: string;
  weight?: number;
  priority?: number;
  max_output_tokens?: number;
  input_cost_per_million?: number;
  output_cost_per_million?: number;
  kind?: string;
  health?: HealthState;
  observed_latency_ms?: number;
}

export interface RoutingPolicy {
  id: string;
  tenant_id?: string;
  name: string;
  description?: string;
  priority: number;
  enabled: boolean;
  match: {
    models?: string[];
    request_types?: string[];
    tenant_ids?: string[];
    api_key_ids?: string[];
    min_prompt_tokens?: number;
    max_prompt_tokens?: number;
    required_capabilities?: string[];
    streaming?: boolean;
  };
  strategy: RoutingStrategy;
  targets: RouteTarget[] | null;
  fallback: { enabled: boolean; max_attempts: number; budget_aware: boolean };
  retry: { max_attempts: number };
  timeout: { total: number; per_attempt: number };
  limits: {
    max_cost_per_request_usd?: number;
    max_prompt_tokens?: number;
    max_output_tokens?: number;
    latency_target_ms?: number;
    requests_per_minute?: number;
    daily_budget_usd?: number;
    monthly_budget_usd?: number;
  };
  version: number;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

export interface Tenant {
  id: string;
  slug: string;
  name: string;
  status: Status;
  plan?: string;
  default_routing_policy_id?: string;
  labels?: Record<string, string>;
  created_at: string;
  updated_at: string;
}

export interface APIKey {
  id: string;
  tenant_id: string;
  name: string;
  prefix: string;
  scopes: string[] | null;
  status: string;
  expires_at?: string;
  last_used_at?: string;
  routing_policy_id?: string;
  created_at: string;
  revoked_at?: string;
  created_by?: string;
}

export interface Budget {
  id: string;
  tenant_id: string;
  scope: string;
  scope_id?: string;
  period: 'daily' | 'weekly' | 'monthly' | 'total';
  limit_usd: number;
  spent_usd: number;
  enforced: boolean;
  alert_thresholds_usd?: number[];
  reset_at: string;
  created_at: string;
  updated_at: string;
}

export interface RequestLog {
  id: string;
  request_id: string;
  trace_id?: string;
  tenant_id?: string;
  api_key_id?: string;
  requested_model?: string;
  routed_model?: string;
  provider?: string;
  policy_id?: string;
  strategy?: string;
  status: number;
  outcome: UsageOutcome;
  error_code?: string;
  error_message?: string;
  latency_ms: number;
  attempts: number;
  fallback_used: boolean;
  prompt_tokens: number;
  completion_tokens: number;
  cost_usd: number;
  client_ip?: string;
  user_agent?: string;
  created_at: string;
}

export interface AuditEvent {
  id: string;
  action: string;
  resource: string;
  resource_id: string;
  tenant_id?: string;
  actor_key_id?: string;
  actor_label?: string;
  actor_ip?: string;
  before?: Record<string, unknown>;
  after?: Record<string, unknown>;
  metadata?: Record<string, string>;
  created_at: string;
}

export interface SystemResponse {
  version: {
    version?: string;
    commit?: string;
    build_date?: string;
    go_version?: string;
    dirty?: boolean;
  };
  uptime: string;
  config: Record<string, unknown>;
  providers: ProviderSummary[] | null;
  components: Record<string, unknown>;
  runtime: {
    goroutines: number;
    go_version: string;
    num_cpu: number;
    gomaxprocs: number;
  };
}

export interface UsageSummaryResponse {
  window: TimeRange;
  summary: UsageSummary;
  providers: ProviderBreakdownRow[] | null;
  models: ModelBreakdownRow[] | null;
}

export interface ErrorsResponse {
  window: TimeRange;
  errors: RequestLog[] | null;
  by_code: Record<string, number>;
  total: number;
}

/** The OpenAI-compatible error envelope the gateway returns for every failure. */
export interface ErrorEnvelope {
  error: {
    message: string;
    type?: string;
    code?: string;
    param?: string | null;
  };
}

// ---------------------------------------------------------------------------
// Phase 2: intelligence and control plane types.
// ---------------------------------------------------------------------------

export type TaskType =
  | 'chat'
  | 'coding'
  | 'summarization'
  | 'extraction'
  | 'reasoning'
  | 'translation'
  | 'tool-use'
  | 'structured_output'
  | 'long_context'
  | 'high_priority_interactive'
  | 'batch_offline'
  | 'unknown';

export interface TaskClassification {
  task: TaskType;
  secondary?: TaskType[];
  confidence: number;
  signals?: string[];
  prompt_tokens: number;
  message_count: number;
  source?: string;
}

export interface ProviderScore {
  id: string;
  provider_id: string;
  provider_name?: string;
  window: string;
  requests: number;
  success_rate: number;
  timeout_rate: number;
  refusal_rate: number;
  avg_latency_ms: number;
  cost_per_success: number;
  fallback_rate: number;
  feedback_score?: number;
  score: number;
  explanation?: string;
  updated_at: string;
}

export interface ModelScore {
  id: string;
  provider_name?: string;
  model: string;
  window: string;
  requests: number;
  success_rate: number;
  avg_latency_ms: number;
  cost_per_success: number;
  score: number;
  explanation?: string;
  updated_at: string;
}

export interface CacheStats {
  exact_hits: number;
  exact_misses: number;
  prefix_hits: number;
  semantic_hits: number;
  bypasses: number;
  hit_rate: number;
  entries?: number;
  /** Phase 5 additions (all optional for backward compatibility). */
  misses?: number;
  bypass_by_reason?: Record<string, number>;
  invalidations?: number;
  reuse_count?: number;
  latency_saved_ms?: number;
  cost_saved_usd?: number;
  lookup_ms_avg?: number;
  top_prompts?: CacheTopPrompt[];
}

export interface CacheTopPrompt {
  prompt_hash: string;
  prompt_preview?: string;
  model?: string;
  hits: number;
}

export interface CachePolicy {
  id: string;
  tenant_id?: string;
  endpoint_id?: string;
  api_key_id?: string;
  provider?: string;
  model?: string;
  name: string;
  enabled: boolean;
  ttl_seconds: number;
  semantic_enabled?: boolean;
  semantic_threshold?: number;
  prefix_enabled?: boolean;
  bypass_tool_requests?: boolean;
  allow_nondeterministic?: boolean;
  created_at: string;
  updated_at: string;
}

export interface CachePolicyInput {
  name: string;
  tenant_id?: string;
  endpoint_id?: string;
  api_key_id?: string;
  provider?: string;
  model?: string;
  enabled?: boolean;
  ttl_seconds?: number;
  semantic_enabled?: boolean;
  semantic_threshold?: number;
  prefix_enabled?: boolean;
  bypass_tool_requests?: boolean;
  allow_nondeterministic?: boolean;
}

export interface CacheEntry {
  id: string;
  tenant_id: string;
  kind: 'exact' | 'prefix' | 'semantic';
  cache_key: string;
  model?: string;
  provider?: string;
  prompt_hash: string;
  prompt_preview?: string;
  hit_count: number;
  reuse_count?: number;
  expires_at?: string;
  created_at?: string;
  updated_at?: string;
}

export interface CacheInvalidation {
  id: string;
  tenant_id?: string;
  scope: string;
  target?: string;
  reason: string;
  actor?: string;
  removed: number;
  created_at: string;
}

export interface ReplayJob {
  id: string;
  tenant_id?: string;
  name?: string;
  status: string;
  request_ids?: string[];
  dataset?: string;
  providers?: string[];
  models?: string[];
  max_requests?: number;
  progress?: number;
  total?: number;
  error?: string;
  created_at: string;
  updated_at: string;
}

export interface EvaluationRun {
  id: string;
  tenant_id?: string;
  replay_job_id?: string;
  dataset?: string;
  status: string;
  error?: string;
  created_at: string;
  finished_at?: string;
}

export interface EvaluationResult {
  id: string;
  evaluation_id: string;
  request_id?: string;
  provider: string;
  model: string;
  score: number;
  latency_ms: number;
  cost_usd: number;
  output_preview?: string;
  is_regression?: boolean;
  metrics?: Record<string, number>;
  created_at: string;
}

export interface Endpoint {
  id: string;
  tenant_id?: string;
  slug: string;
  name: string;
  description?: string;
  routing_override?: {
    strategy?: RoutingStrategy;
    preferred_models?: string[];
    preferred_providers?: string[];
    max_cost_usd?: number;
    latency_target_ms?: number;
    use_cache?: boolean;
    force_model?: string;
    block_fallback?: boolean;
  };
  enabled: boolean;
  created_at: string;
  updated_at: string;
}

export interface RoutingExplanation {
  request_id?: string;
  task: TaskClassification;
  policy?: Record<string, unknown>;
  decision?: Record<string, unknown> & { reason?: string };
  shaping?: Record<string, unknown>;
  cache?: { hit: boolean; kind?: string };
  reason: string;
}

// ---------------------------------------------------------------------------
// Phase 4: tools, policies, invocations and agent runs.
// ---------------------------------------------------------------------------

export type ToolKind = 'builtin' | 'external';
export type ToolOwner = 'platform' | 'tenant';
export type ToolSafetyLevel = 'safe' | 'sensitive' | 'dangerous';
export type ToolPolicyMode = 'disabled' | 'manual' | 'automatic';
export type ToolInvocationStatus =
  | 'pending'
  | 'executed'
  | 'skipped'
  | 'denied'
  | 'failed'
  | 'superseded';
export type ToolRunStatus =
  | 'none'
  | 'client_executed'
  | 'gateway_executed'
  | 'gateway_failed'
  | 'denied';
export type AgentRunStatus =
  | 'running'
  | 'completed'
  | 'step_limit'
  | 'call_limit'
  | 'time_limit'
  | 'failed'
  | 'denied';

export interface Tool {
  id: string;
  name: string;
  description?: string;
  kind: ToolKind;
  owner: ToolOwner;
  tenant_id?: string;
  parameters?: Record<string, unknown>;
  strict?: boolean;
  safety_level: ToolSafetyLevel;
  executable: boolean;
  handler?: string;
  version?: string;
  enabled: boolean;
  requires_approval: boolean;
  labels?: Record<string, string>;
  created_at: string;
  updated_at: string;
}

export interface ToolInput {
  name: string;
  description?: string;
  kind?: ToolKind;
  owner?: ToolOwner;
  tenant_id?: string;
  parameters?: Record<string, unknown>;
  strict?: boolean;
  safety_level?: ToolSafetyLevel;
  handler?: string;
  version?: string;
  enabled?: boolean;
  requires_approval?: boolean;
  labels?: Record<string, string>;
}

export interface ToolPolicy {
  id: string;
  tenant_id?: string;
  name: string;
  enabled: boolean;
  mode: ToolPolicyMode;
  allowed_tools?: string[];
  denied_tools?: string[];
  allowed_providers?: string[];
  denied_providers?: string[];
  require_approval: boolean;
  max_steps: number;
  max_tool_calls: number;
  max_result_bytes: number;
  max_run_seconds: number;
  block_sensitive: boolean;
  created_at: string;
  updated_at: string;
}

export interface ToolPolicyInput {
  name: string;
  tenant_id?: string;
  enabled?: boolean;
  mode?: ToolPolicyMode;
  allowed_tools?: string[];
  denied_tools?: string[];
  allowed_providers?: string[];
  denied_providers?: string[];
  require_approval?: boolean;
  max_steps?: number;
  max_tool_calls?: number;
  max_result_bytes?: number;
  max_run_seconds?: number;
  block_sensitive?: boolean;
}

export interface ToolInvocation {
  id: string;
  request_id: string;
  tenant_id?: string;
  run_id?: string;
  step: number;
  tool_call_id: string;
  tool_name: string;
  arguments?: string;
  status: ToolInvocationStatus;
  deny_reason?: string;
  latency_ms: number;
  result_bytes: number;
  error_code?: string;
  provider?: string;
  model?: string;
  created_at: string;
}

export interface AgentRun {
  id: string;
  request_id: string;
  tenant_id?: string;
  status: AgentRunStatus;
  steps: number;
  tool_calls: number;
  provider?: string;
  model?: string;
  latency_ms: number;
  stop_reason?: string;
  created_at: string;
  updated_at: string;
}

export interface AgentStep {
  id: string;
  run_id: string;
  step: number;
  kind: 'model' | 'tool';
  provider?: string;
  model?: string;
  tool_calls: number;
  latency_ms: number;
  tokens: number;
  detail?: Record<string, unknown>;
  created_at: string;
}

// ToolRunMeta is the per-response tool summary carried in synapass.tool_run.
export interface ToolRunMeta {
  mode: string;
  status: ToolRunStatus;
  run_id?: string;
  steps: number;
  calls: number;
  executed: number;
  skipped: number;
  denied: number;
  failed: number;
  tools?: string[];
  stop_reason?: string;
  latency_ms: number;
}
// ---------------------------------------------------------------------------
// Temporary public tunnels (Cloudflare quick tunnels).
// ---------------------------------------------------------------------------

export type TunnelStatus = 'starting' | 'running' | 'stopped' | 'failed';

export interface TunnelSession {
  id: string;
  tenant_id?: string;
  target: string;
  target_addr: string;
  public_url?: string;
  status: TunnelStatus;
  started_at?: string;
  url_at?: string;
  stopped_at?: string;
  stop_reason?: string;
  last_error?: string;
  reconnects: number;
  created_by?: string;
  created_at: string;
  updated_at: string;
}

export interface TunnelStatusResponse {
  enabled: boolean;
  binary_available: boolean;
  binary_path?: string;
  binary_error?: string;
  active: TunnelSession | null;
}

export interface TunnelCurrentURL {
  url: string | null;
  status: TunnelStatus | null;
  target: string | null;
  target_addr: string | null;
  session_id?: string;
  started_at?: string;
}

// ---------------------------------------------------------------------------
// Cost intelligence
// ---------------------------------------------------------------------------

export interface CostOverview {
  window_from: string;
  window_to: string;
  requests: number;
  actual_usd: number;
  estimated_usd: number;
  estimate_accuracy: number;
  tokens: number;
  cost_per_kilo_token_usd: number;
  cache_savings_usd: number;
  routing_savings_usd: number;
  generated_at: string;
}

export interface CostOverviewResponse {
  overview: CostOverview;
}

export interface CostDimensionRow {
  key: string;
  requests: number;
  tokens: number;
  actual_usd: number;
  estimated_usd: number;
  share: number;
}

export interface CostGroupedResponse {
  dimension: string;
  rows: CostDimensionRow[];
}

export interface CostPoint {
  bucket: string;
  requests: number;
  actual_usd: number;
  estimated_usd: number;
}

export interface CostSeriesResponse {
  series: CostPoint[];
}

export interface CostLineItem {
  kind: string;
  label: string;
  quantity: number;
  unit_price_usd: number;
  amount_usd: number;
}

export interface CostBreakdown {
  lines: CostLineItem[];
  total_usd: number;
  currency: string;
  pricing_version_id?: string;
  pricing_source?: string;
  estimated: boolean;
}

export interface CostRecord {
  id: string;
  request_id: string;
  tenant_id: string;
  provider: string;
  model: string;
  requested_model?: string;
  endpoint_id?: string;
  request_type: string;
  usage: {
    prompt_tokens: number;
    completion_tokens: number;
    total_tokens: number;
    cached_prompt_tokens: number;
    estimated: boolean;
  };
  cost: { usd: number };
  estimate_cost: { usd: number };
  pricing_version_id?: string;
  pricing_source?: string;
  breakdown?: CostBreakdown;
  outcome: string;
  cache_hit: boolean;
  fallback_used: boolean;
  streaming: boolean;
  status: number;
  created_at: string;
}

export interface CostRequestResponse {
  record: CostRecord;
  estimate_accuracy: number;
}

export interface CostTopResponse {
  requests: CostRecord[];
}

export interface PricingVersion {
  id: string;
  scope: string;
  scope_id?: string;
  currency: string;
  input_cost_per_million: number;
  output_cost_per_million: number;
  cached_input_cost_per_million?: number;
  base_fee_usd?: number;
  effective_from: string;
  effective_to?: string;
  created_by?: string;
  created_at: string;
}

export interface PricingListResponse {
  pricing: PricingVersion[];
}

export interface BudgetStatus {
  budget_id: string;
  tenant_id: string;
  scope: string;
  period: string;
  limit_usd: number;
  spent_usd: number;
  remaining_usd: number;
  utilization: number;
  burn_rate: number;
  projected_usd: number;
  exhausted: boolean;
  enforced: boolean;
  reset_at: string;
}

export interface BudgetAlert {
  id: string;
  budget_id: string;
  tenant_id: string;
  threshold_usd: number;
  spent_usd: number;
  limit_usd: number;
  period: string;
  period_start: string;
  fired_at: string;
}

export interface CostBudgetsResponse {
  budgets: BudgetStatus[];
  alerts: BudgetAlert[];
}

export interface CostAnomaly {
  id: string;
  dimension: string;
  key: string;
  window_from: string;
  window_to: string;
  observed_usd: number;
  expected_usd: number;
  ratio: number;
  severity: 'info' | 'warning' | 'critical';
  detected_at: string;
}

export interface CostAnomaliesResponse {
  anomalies: CostAnomaly[];
}

export interface CostForecast {
  daily_average_usd: number;
  trend_usd_per_day: number;
  projected_month_usd: number;
  projected_month_low_usd: number;
  projected_month_high_usd: number;
  days_observed: number;
  generated_at: string;
}

export interface CostForecastResponse {
  forecast: CostForecast;
}

export interface CostSavings {
  cache_usd: number;
  routing_usd: number;
  fallback_usd: number;
}

export interface CostSavingsResponse {
  savings: CostSavings;
  total_usd: number;
}
