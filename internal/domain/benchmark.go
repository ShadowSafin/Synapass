// Package domain benchmark types: the Autonomous Benchmark Engine's vocabulary.
//
// The engine continuously benchmarks providers and models, stores every
// result permanently, and powers a Grafana-style analytics dashboard. All
// types are workspace (tenant) scoped: a benchmark row always carries the
// tenant that owns it, and every query filters on it.
package domain

import "time"

// Benchmark schedule presets supported by the scheduler, plus custom cron.
const (
	BenchmarkEveryMinute   = "every_minute"
	BenchmarkEvery5Minutes = "every_5m"
	BenchmarkEvery15Min    = "every_15m"
	BenchmarkEvery30Min    = "every_30m"
	BenchmarkEveryHour     = "every_hour"
	BenchmarkEvery3Hours   = "every_3h"
	BenchmarkEvery6Hours   = "every_6h"
	BenchmarkEvery12Hours  = "every_12h"
	BenchmarkEvery24Hours  = "every_24h"
	BenchmarkCustomCron    = "custom"
)

// BenchmarkScheduleIntervals maps each preset to its tick duration.
// Custom cron expressions are parsed separately.
func BenchmarkScheduleIntervals() map[string]time.Duration {
	return map[string]time.Duration{
		BenchmarkEveryMinute:   time.Minute,
		BenchmarkEvery5Minutes: 5 * time.Minute,
		BenchmarkEvery15Min:    15 * time.Minute,
		BenchmarkEvery30Min:    30 * time.Minute,
		BenchmarkEveryHour:     time.Hour,
		BenchmarkEvery3Hours:   3 * time.Hour,
		BenchmarkEvery6Hours:   6 * time.Hour,
		BenchmarkEvery12Hours:  12 * time.Hour,
		BenchmarkEvery24Hours:  24 * time.Hour,
	}
}

// BenchmarkPromptCategory classifies a benchmark prompt.
type BenchmarkPromptCategory string

const (
	BenchmarkCatCoding          BenchmarkPromptCategory = "coding"
	BenchmarkCatReasoning       BenchmarkPromptCategory = "reasoning"
	BenchmarkCatMath            BenchmarkPromptCategory = "math"
	BenchmarkCatTranslation     BenchmarkPromptCategory = "translation"
	BenchmarkCatVision          BenchmarkPromptCategory = "vision"
	BenchmarkCatToolCalling     BenchmarkPromptCategory = "tool_calling"
	BenchmarkCatJSONMode        BenchmarkPromptCategory = "json_mode"
	BenchmarkCatCreative        BenchmarkPromptCategory = "creative_writing"
	BenchmarkCatLongContext     BenchmarkPromptCategory = "long_context"
	BenchmarkCatFunctionCalling BenchmarkPromptCategory = "function_calling"
)

// BenchmarkPromptDifficulty grades a prompt.
type BenchmarkPromptDifficulty string

const (
	BenchmarkDifficultyEasy   BenchmarkPromptDifficulty = "easy"
	BenchmarkDifficultyMedium BenchmarkPromptDifficulty = "medium"
	BenchmarkDifficultyHard   BenchmarkPromptDifficulty = "hard"
)

// BenchmarkPrompt is one probe in a dataset.
type BenchmarkPrompt struct {
	ID           string                    `json:"id"`
	DatasetID    string                    `json:"dataset_id,omitempty"`
	Prompt       string                    `json:"prompt"`
	Category     BenchmarkPromptCategory   `json:"category"`
	ExpectedJSON string                    `json:"expected_json,omitempty"`
	Difficulty   BenchmarkPromptDifficulty `json:"difficulty"`
	Weight       float64                   `json:"weight"`
	MaxTokens    int                       `json:"max_tokens"`
	Temperature  float64                   `json:"temperature"`
	CreatedAt    time.Time                 `json:"created_at"`
}

// BenchmarkDataset groups prompts. One default dataset ships with the engine.
type BenchmarkDataset struct {
	ID          string            `json:"id"`
	TenantID    string            `json:"tenant_id,omitempty"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	IsDefault   bool              `json:"is_default"`
	Prompts     []BenchmarkPrompt `json:"prompts,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// BenchmarkJob is a user-created continuous benchmarking job.
type BenchmarkJob struct {
	ID                string            `json:"id"`
	TenantID          string            `json:"tenant_id,omitempty"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	Enabled           bool              `json:"enabled"`
	Providers         []string          `json:"providers"`
	Models            []string          `json:"models"`
	DatasetID         string            `json:"dataset_id,omitempty"`
	Schedule          string            `json:"schedule"`
	CronExpression    string            `json:"cron_expression,omitempty"`
	TimeoutMS         int64             `json:"timeout_ms"`
	RetryCount        int               `json:"retry_count"`
	Region            string            `json:"region,omitempty"`
	Concurrency       int               `json:"concurrency"`
	MaxDailyBudgetUSD float64           `json:"max_daily_budget_usd"`
	Tags              map[string]string `json:"tags,omitempty"`
	LastRunAt         *time.Time        `json:"last_run_at,omitempty"`
	NextRunAt         *time.Time        `json:"next_run_at,omitempty"`
	CreatedAt         time.Time         `json:"created_at"`
	UpdatedAt         time.Time         `json:"updated_at"`
}

// BenchmarkRun is one scheduled execution of a job.
type BenchmarkRun struct {
	ID           string     `json:"id"`
	JobID        string     `json:"job_id"`
	TenantID     string     `json:"tenant_id,omitempty"`
	Status       string     `json:"status"`
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	DurationMS   int64      `json:"duration_ms"`
	TotalProbes  int        `json:"total_probes"`
	SuccessCount int        `json:"success_count"`
	FailureCount int        `json:"failure_count"`
	TotalCostUSD float64    `json:"total_cost_usd"`
	TotalTokens  int64      `json:"total_tokens"`
	Error        string     `json:"error,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

// BenchmarkResult is one provider/model/prompt measurement. Never overwritten.
type BenchmarkResult struct {
	ID                 string            `json:"id"`
	RunID              string            `json:"run_id"`
	JobID              string            `json:"job_id"`
	TenantID           string            `json:"tenant_id,omitempty"`
	Provider           string            `json:"provider"`
	Model              string            `json:"model"`
	PromptID           string            `json:"prompt_id,omitempty"`
	Category           string            `json:"category,omitempty"`
	TTFTMS             int64             `json:"ttft_ms"`
	LatencyMS          int64             `json:"latency_ms"`
	TokensPerSecond    float64           `json:"tokens_per_second"`
	PromptTokens       int               `json:"prompt_tokens"`
	CompletionTokens   int               `json:"completion_tokens"`
	TotalTokens        int               `json:"total_tokens"`
	CostUSD            float64           `json:"cost_usd"`
	Success            bool              `json:"success"`
	Error              string            `json:"error,omitempty"`
	ErrorType          string            `json:"error_type,omitempty"`
	HTTPStatus         int               `json:"http_status"`
	RetryCount         int               `json:"retry_count"`
	FinishReason       string            `json:"finish_reason,omitempty"`
	StreamingSpeed     float64           `json:"streaming_speed_tps,omitempty"`
	JSONSuccess        *bool             `json:"json_success,omitempty"`
	ToolCallingSuccess *bool             `json:"tool_calling_success,omitempty"`
	ContextLength      int               `json:"context_length"`
	ProviderMetadata   map[string]string `json:"provider_metadata,omitempty"`
	CreatedAt          time.Time         `json:"created_at"`
}

// BenchmarkReport aggregates a window: daily, weekly or monthly.
type BenchmarkReport struct {
	ID                 string         `json:"id"`
	TenantID           string         `json:"tenant_id,omitempty"`
	Period             string         `json:"period"`
	PeriodStart        time.Time      `json:"period_start"`
	PeriodEnd          time.Time      `json:"period_end"`
	FastestProvider    string         `json:"fastest_provider,omitempty"`
	CheapestProvider   string         `json:"cheapest_provider,omitempty"`
	MostReliable       string         `json:"most_reliable_provider,omitempty"`
	LowestLatency      string         `json:"lowest_latency_provider,omitempty"`
	HighestThroughput  string         `json:"highest_throughput_provider,omitempty"`
	HighestSuccess     string         `json:"highest_success_rate_provider,omitempty"`
	BiggestRegression  string         `json:"biggest_regression,omitempty"`
	BiggestImprovement string         `json:"biggest_improvement,omitempty"`
	OverallWinner      string         `json:"overall_winner,omitempty"`
	Summary            map[string]any `json:"summary,omitempty"`
	CreatedAt          time.Time      `json:"created_at"`
}

// BenchmarkProviderHealth is the health dashboard row for one provider.
type BenchmarkProviderHealth struct {
	Provider        string     `json:"provider"`
	Status          string     `json:"status"`
	Uptime          float64    `json:"uptime"`
	AvgLatencyMS    float64    `json:"avg_latency_ms"`
	AvgTTFTMS       float64    `json:"avg_ttft_ms"`
	AvgCostUSD      float64    `json:"avg_cost_usd"`
	AvgTPS          float64    `json:"avg_tokens_per_second"`
	AvgStreamingTPS float64    `json:"avg_streaming_tps"`
	SuccessRate     float64    `json:"success_rate"`
	LastFailure     *time.Time `json:"last_failure,omitempty"`
	LastBenchmark   *time.Time `json:"last_benchmark,omitempty"`
	TotalProbes     int64      `json:"total_probes"`
}

// BenchmarkInsight is one generated natural-language summary.
type BenchmarkInsight struct {
	Text      string    `json:"text"`
	Severity  string    `json:"severity,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Metric    string    `json:"metric,omitempty"`
	DeltaPct  float64   `json:"delta_pct,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// BenchmarkOverview is the dashboard header: engine, scheduler, queue and totals.
type BenchmarkOverview struct {
	EngineEnabled     bool       `json:"engine_enabled"`
	EngineStatus      string     `json:"engine_status"`
	SchedulerStatus   string     `json:"scheduler_status"`
	ActiveJobs        int        `json:"active_jobs"`
	RunningWorkers    int        `json:"running_workers"`
	QueueSize         int        `json:"queue_size"`
	LastBenchmark     *time.Time `json:"last_benchmark,omitempty"`
	NextBenchmark     *time.Time `json:"next_benchmark,omitempty"`
	AvgDurationMS     float64    `json:"avg_benchmark_duration_ms"`
	TotalBenchmarks   int64      `json:"total_benchmarks"`
	SuccessRate       float64    `json:"success_rate"`
	FailedJobs        int64      `json:"failed_jobs"`
	TotalDailyCostUSD float64    `json:"total_daily_cost_usd"`
	TotalTokens       int64      `json:"total_tokens_benchmarked"`
}

// BenchmarkNotificationChannel names a delivery channel.
type BenchmarkNotificationChannel string

const (
	BenchmarkNotifyEmail   BenchmarkNotificationChannel = "email"
	BenchmarkNotifyDiscord BenchmarkNotificationChannel = "discord"
	BenchmarkNotifySlack   BenchmarkNotificationChannel = "slack"
)

// BenchmarkNotification is a delivery rule plus its event triggers.
type BenchmarkNotification struct {
	ID        string                       `json:"id"`
	TenantID  string                       `json:"tenant_id,omitempty"`
	Channel   BenchmarkNotificationChannel `json:"channel"`
	Target    string                       `json:"target"`
	Events    []string                     `json:"events"`
	Enabled   bool                         `json:"enabled"`
	CreatedAt time.Time                    `json:"created_at"`
	UpdatedAt time.Time                    `json:"updated_at"`
}

// BenchmarkWorkerLog is one worker lifecycle event.
type BenchmarkWorkerLog struct {
	ID        string    `json:"id"`
	WorkerID  string    `json:"worker_id"`
	RunID     string    `json:"run_id,omitempty"`
	Level     string    `json:"level"`
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// Benchmark audit resources.
const (
	ResourceBenchmarkJob          AuditResource = "benchmark_job"
	ResourceBenchmarkRun          AuditResource = "benchmark_run"
	ResourceBenchmarkDataset      AuditResource = "benchmark_dataset"
	ResourceBenchmarkReport       AuditResource = "benchmark_report"
	ResourceBenchmarkNotification AuditResource = "benchmark_notification"
)
