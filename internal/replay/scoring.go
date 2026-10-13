package replay

import (
	"context"
	"log/slog"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/eval"
)

// SubjectReplayCompleted carries a finished replay job: its status, what ran,
// and why it stopped. Published best-effort on the core NATS subject below so
// workers and the dashboard can observe completion without polling the API.
// Delivery is advisory only: like every other NATS publish, a broker outage
// must never fail a job that already completed.
const SubjectReplayCompleted = "ar.replay.completed"

// Scorer scores one replayed output against a reference answer, returning the
// composite score and the per-metric breakdown.
type Scorer interface {
	Score(ctx context.Context, reference, candidate string) (score float64, metrics map[string]float64, err error)
}

// EvalScorer is the default Scorer. It delegates to the deterministic eval
// engine, so replay scores are byte-for-byte reproducible and agree with the
// golden-case and comparison paths rather than coming from a second,
// divergent scorer.
type EvalScorer struct{}

// Score implements Scorer.
func (EvalScorer) Score(_ context.Context, reference, candidate string) (float64, map[string]float64, error) {
	score, metrics := eval.Score(reference, candidate)
	return score, metrics, nil
}

// ReferenceSource returns the golden reference answer for one captured
// request, for scoring replayed outputs against. It reports false when no
// reference is known (the common case: dataset replay is not supported yet,
// so most jobs have no goldens). A missing reference is not an error: the
// caller scores against the empty string, which deterministically yields zero
// similarity with the metrics still populated to prove the score was
// computed rather than defaulted.
type ReferenceSource interface {
	GetReference(ctx context.Context, dataset, requestID string) (reference string, ok bool, err error)
}

// Publisher publishes completion events. *storage.NATS satisfies it via
// PublishJSON; tests wire a stub. Every method on the real implementation is
// nil-receiver safe, and callers additionally skip the publish when no
// publisher is configured, so a gateway without NATS runs replay unchanged.
type Publisher interface {
	PublishJSON(subject string, value any)
}

// CompletionEvent is the replay job completion payload published on
// SubjectReplayCompleted.
type CompletionEvent struct {
	JobID      string    `json:"job_id"`
	RunID      string    `json:"run_id"`
	TenantID   string    `json:"tenant_id,omitempty"`
	Status     string    `json:"status"`
	Error      string    `json:"error,omitempty"`
	Total      int       `json:"total"`
	Succeeded  int       `json:"succeeded"`
	FinishedAt time.Time `json:"finished_at"`
}

// scoreOutput computes the similarity score for one successful execution,
// merging the scorer's per-metric breakdown over the caller's base metrics
// (token counts). A scorer failure degrades to score zero with the base
// metrics intact rather than failing the result: scoring must never lose a
// provider output that already executed.
func scoreOutput(ctx context.Context, scorer Scorer, logger *slog.Logger, reference, candidate string, base map[string]float64) (float64, map[string]float64) {
	if scorer == nil {
		scorer = EvalScorer{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	merged := map[string]float64{}
	for k, v := range base {
		merged[k] = v
	}
	score, metrics, err := scorer.Score(ctx, reference, candidate)
	if err != nil {
		logger.Warn("replay scoring failed; storing unscored result", "error", err)
		return 0, merged
	}
	for k, v := range metrics {
		merged[k] = v
	}
	return score, merged
}

// lookupReference returns the golden reference for one request, or the empty
// string when none is known or the lookup fails. Nil sources are the normal
// production state until dataset replay lands.
func lookupReference(ctx context.Context, refs ReferenceSource, dataset, requestID string) string {
	if refs == nil {
		return ""
	}
	ref, ok, err := refs.GetReference(ctx, dataset, requestID)
	if err != nil || !ok {
		return ""
	}
	return ref
}

// publishCompletion emits the job completion event best-effort. A nil
// publisher (gateway without NATS) skips silently; the typed-nil *storage.NATS
// case is safe too because PublishJSON is nil-receiver safe.
func publishCompletion(events Publisher, job *domain.ReplayJob, run *domain.EvaluationRun, succeeded int) {
	if events == nil || job == nil || run == nil {
		return
	}
	events.PublishJSON(SubjectReplayCompleted, CompletionEvent{
		JobID: job.ID, RunID: run.ID, TenantID: job.TenantID,
		Status: run.Status, Error: run.Error,
		Total: job.Total, Succeeded: succeeded,
		FinishedAt: domain.Now(),
	})
}
