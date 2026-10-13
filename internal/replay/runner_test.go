package replay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

type fakeStore struct {
	jobs    map[string]*domain.ReplayJob
	runs    map[string]*domain.EvaluationRun
	results []*domain.EvaluationResult
}

func newFakeStore() *fakeStore {
	return &fakeStore{jobs: map[string]*domain.ReplayJob{}, runs: map[string]*domain.EvaluationRun{}}
}

func (f *fakeStore) CreateJob(_ context.Context, job *domain.ReplayJob) error {
	f.jobs[job.ID] = job
	return nil
}
func (f *fakeStore) UpdateJob(_ context.Context, job *domain.ReplayJob) error {
	f.jobs[job.ID] = job
	return nil
}
func (f *fakeStore) GetJob(_ context.Context, id string) (*domain.ReplayJob, error) {
	return f.jobs[id], nil
}
func (f *fakeStore) ListJobs(_ context.Context, _ string, _ int) ([]domain.ReplayJob, error) {
	return nil, nil
}
func (f *fakeStore) CreateRun(_ context.Context, run *domain.EvaluationRun) error {
	f.runs[run.ID] = run
	return nil
}
func (f *fakeStore) UpdateRun(_ context.Context, run *domain.EvaluationRun) error {
	f.runs[run.ID] = run
	return nil
}
func (f *fakeStore) AddResult(_ context.Context, res *domain.EvaluationResult) error {
	f.results = append(f.results, res)
	return nil
}
func (f *fakeStore) ListResults(_ context.Context, _ string, _ int) ([]domain.EvaluationResult, error) {
	return nil, nil
}

type fakeLogs struct {
	entries map[string]*domain.RequestLog
}

func (f *fakeLogs) GetByRequestID(_ context.Context, id string) (*domain.RequestLog, error) {
	return f.entries[id], nil
}
func (f *fakeLogs) List(_ context.Context, _ ListFilter) ([]domain.RequestLog, error) {
	return nil, nil
}

type fakePayloads struct {
	entries map[string]*domain.RequestPayload
}

func (f *fakePayloads) Get(_ context.Context, id string) (*domain.RequestPayload, error) {
	return f.entries[id], nil
}

type fakeAdapter struct {
	name    string
	enabled bool
	output  string
	usage   domain.TokenUsage
	err     error
	calls   int
}

func (f *fakeAdapter) Name() string                       { return f.name }
func (f *fakeAdapter) Kind() domain.ProviderKind          { return domain.ProviderOpenAICompatible }
func (f *fakeAdapter) Capabilities() domain.CapabilitySet { return domain.CapabilitySet{} }
func (f *fakeAdapter) HealthCheck(context.Context) domain.ProviderHealth {
	return domain.ProviderHealth{State: domain.HealthHealthy}
}
func (f *fakeAdapter) Enabled() bool { return f.enabled }
func (f *fakeAdapter) ChatCompletion(_ context.Context, req *providers.Request) (*providers.Response, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &providers.Response{
		Model:   req.Model,
		Choices: []domain.Choice{{Message: &domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(f.output)}}},
		Usage:   f.usage,
	}, nil
}
func (f *fakeAdapter) ChatCompletionStream(_ context.Context, req *providers.Request, _ providers.StreamHandler) (*providers.Response, error) {
	return f.ChatCompletion(context.Background(), req)
}

type fakeAdapters struct {
	byName map[string]*fakeAdapter
}

func (f *fakeAdapters) Resolve(idOrName string) (providers.Adapter, bool) {
	a, ok := f.byName[idOrName]
	if !ok || a == nil {
		return nil, false
	}
	return a, true
}

// stubPricer prices at 0.5/M input and 2.0/M output, the sheet the cost
// test below asserts against.
func stubPricer(_ context.Context, _, _, _ string, usage domain.TokenUsage) float64 {
	return float64(usage.PromptTokens)/1_000_000*0.5 +
		float64(usage.CompletionTokens)/1_000_000*2.0
}

func testPayload() *domain.RequestPayload {
	return &domain.RequestPayload{
		RequestID: "req_1",
		Model:     "flash",
		Messages:  []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("hi")}},
	}
}

func TestRunnerReplaysAgainstOriginalTarget(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued", RequestIDs: []string{"req_1"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	adapter := &fakeAdapter{name: "bynara", enabled: true, output: "hello",
		usage: domain.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{"req_1": testPayload()}},
		Logs:     &fakeLogs{entries: map[string]*domain.RequestLog{"req_1": {RequestID: "req_1", Provider: "bynara", RequestedModel: "flash", RoutedModel: "flash"}}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{"bynara": adapter}},
		Pricer:   stubPricer,
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("status = %q, want completed (error %q)", run.Status, run.Error)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
	if len(store.results) != 1 {
		t.Fatalf("results = %d, want 1", len(store.results))
	}
	res := store.results[0]
	if res.EvaluationID != run.ID {
		t.Error("result is not linked to the run")
	}
	if res.OutputPreview != "hello" {
		t.Errorf("preview = %q, want hello", res.OutputPreview)
	}
	// 10 prompt + 5 completion at 0.5/2.0 per million = 0.000015.
	if diff := res.CostUSD - 0.000015; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %v, want 0.000015", res.CostUSD)
	}
	if res.Metrics["total_tokens"] != 15 {
		t.Errorf("total_tokens metric = %v, want 15", res.Metrics["total_tokens"])
	}
	if job.Status != "completed" || job.Progress != 1 || job.Total != 1 {
		t.Errorf("job = %+v, want completed 1/1", job)
	}
}

func TestRunnerCrossProductTargets(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued", RequestIDs: []string{"req_1"},
		Providers: []string{"a", "b"}, Models: []string{"m1", "m2"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	a := &fakeAdapter{name: "a", enabled: true, output: "A"}
	b := &fakeAdapter{name: "b", enabled: true, output: "B"}
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{"req_1": testPayload()}},
		Logs:     &fakeLogs{entries: map[string]*domain.RequestLog{"req_1": {RequestID: "req_1", Provider: "a", RequestedModel: "m1"}}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{"a": a, "b": b}},
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("status = %q (%q)", run.Status, run.Error)
	}
	if a.calls+b.calls != 4 {
		t.Fatalf("calls = %d, want 4 (2 providers x 2 models)", a.calls+b.calls)
	}
	if job.Total != 4 || job.Progress != 4 {
		t.Errorf("job total/progress = %d/%d, want 4/4", job.Total, job.Progress)
	}
}

func TestRunnerMissingPayloadRecordsErrorResult(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued", RequestIDs: []string{"req_1"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	adapter := &fakeAdapter{name: "a", enabled: true, output: "A"}
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{}},
		Logs:     &fakeLogs{entries: map[string]*domain.RequestLog{"req_1": {RequestID: "req_1", Provider: "a", RequestedModel: "m"}}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{"a": adapter}},
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	// Nothing executed successfully, but the failure is recorded, not silent.
	if run.Status != "failed" {
		t.Fatalf("status = %q, want failed", run.Status)
	}
	if adapter.calls != 0 {
		t.Fatalf("adapter calls = %d, want 0", adapter.calls)
	}
	if len(store.results) != 1 {
		t.Fatalf("results = %d, want 1 error row", len(store.results))
	}
	if job.Status != "failed" || job.Progress != 1 {
		t.Errorf("job = %+v, want failed 1/1", job)
	}
}

func TestRunnerUnknownProvider(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued", RequestIDs: []string{"req_1"},
		Providers: []string{"ghost"}, Models: []string{"m"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{"req_1": testPayload()}},
		Logs:     &fakeLogs{entries: map[string]*domain.RequestLog{"req_1": {RequestID: "req_1", Provider: "a", RequestedModel: "m"}}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{}},
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "failed" {
		t.Fatalf("status = %q, want failed", run.Status)
	}
	if !contains(store.results[0].OutputPreview, "unknown provider") {
		t.Errorf("preview = %q, want unknown-provider error", store.results[0].OutputPreview)
	}
}

func TestRunnerProviderErrorIsPartialSuccess(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued",
		RequestIDs: []string{"req_1", "req_2"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	bad := &fakeAdapter{name: "a", enabled: true, err: errors.New("boom")}
	good := &fakeAdapter{name: "b", enabled: true, output: "ok",
		usage: domain.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{
			"req_1": testPayload(),
			"req_2": {RequestID: "req_2", Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("yo")}}},
		}},
		Logs: &fakeLogs{entries: map[string]*domain.RequestLog{
			"req_1": {RequestID: "req_1", Provider: "a", RequestedModel: "m"},
			"req_2": {RequestID: "req_2", Provider: "b", RequestedModel: "m"},
		}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{"a": bad, "b": good}},
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	// req_1 failed upstream, req_2 succeeded. Both record rows and the job
	// completes with the failure noted on the run.
	if len(store.results) != 2 {
		t.Fatalf("results = %d, want 2", len(store.results))
	}
	if run.Status != "completed" {
		t.Fatalf("status = %q, want completed", run.Status)
	}
	if run.Error == "" {
		t.Error("run error should note the failed execution")
	}
	if job.Progress != 2 || job.Total != 2 {
		t.Errorf("job = %+v, want 2/2", job)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

type fakeRefs struct {
	refs map[string]string
	err  error
}

func (f *fakeRefs) GetReference(_ context.Context, _, requestID string) (string, bool, error) {
	if f == nil {
		return "", false, nil
	}
	if f.err != nil {
		return "", false, f.err
	}
	ref, ok := f.refs[requestID]
	return ref, ok, nil
}

type fakePublisher struct {
	subjects []string
	payloads []any
}

func (f *fakePublisher) PublishJSON(subject string, value any) {
	f.subjects = append(f.subjects, subject)
	f.payloads = append(f.payloads, value)
}

type errScorer struct{ err error }

func (e errScorer) Score(_ context.Context, _, _ string) (float64, map[string]float64, error) {
	return 0, nil, e.err
}

func TestRunnerScoresSuccessfulResults(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued", RequestIDs: []string{"req_1"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	adapter := &fakeAdapter{name: "bynara", enabled: true, output: "hello",
		usage: domain.TokenUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15}}
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{"req_1": testPayload()}},
		Logs:     &fakeLogs{entries: map[string]*domain.RequestLog{"req_1": {RequestID: "req_1", Provider: "bynara", RequestedModel: "flash", RoutedModel: "flash"}}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{"bynara": adapter}},
		Pricer:   stubPricer,
		Scorer:   EvalScorer{},
		References: &fakeRefs{refs: map[string]string{
			"req_1": "hello",
		}},
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("status = %q, want completed (error %q)", run.Status, run.Error)
	}
	if len(store.results) != 1 {
		t.Fatalf("results = %d, want 1", len(store.results))
	}
	res := store.results[0]
	// Exact-match output: the Newton sqrt in the trigram cosine costs an
	// epsilon, so compare approximately.
	if diff := 1 - res.Score; diff < 0 || diff > 1e-9 {
		t.Errorf("score = %v, want ~1 for an exact-match output", res.Score)
	}
	if res.Metrics["exact_match"] != 1 || res.Metrics["token_f1"] != 1 || res.Metrics["jaccard"] != 1 {
		t.Errorf("exact/token/jaccard metrics = %v, want all 1", res.Metrics)
	}
	if diff := 1 - res.Metrics["char_ngram"]; diff < 0 || diff > 1e-9 {
		t.Errorf("char_ngram metric = %v, want ~1", res.Metrics["char_ngram"])
	}
	// Usage metrics survive alongside the similarity breakdown.
	if res.Metrics["total_tokens"] != 15 {
		t.Errorf("total_tokens metric = %v, want 15", res.Metrics["total_tokens"])
	}
}

func TestRunnerScoringWithoutReferenceStillFillsMetrics(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued", RequestIDs: []string{"req_1"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	adapter := &fakeAdapter{name: "a", enabled: true, output: "hello",
		usage: domain.TokenUsage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{"req_1": testPayload()}},
		Logs:     &fakeLogs{entries: map[string]*domain.RequestLog{"req_1": {RequestID: "req_1", Provider: "a", RequestedModel: "m"}}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{"a": adapter}},
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("status = %q, want completed", run.Status)
	}
	res := store.results[0]
	// No golden reference exists, so similarity is a computed zero -- but the
	// breakdown must be present to prove the scorer ran.
	if res.Metrics == nil {
		t.Fatal("metrics are nil; the scorer did not run")
	}
	for _, k := range []string{"exact_match", "token_f1", "jaccard", "char_ngram"} {
		v, ok := res.Metrics[k]
		if !ok {
			t.Errorf("metric %q is missing", k)
			continue
		}
		if v != 0 {
			t.Errorf("metric %q = %v, want computed 0", k, v)
		}
	}
	if res.Metrics["prompt_tokens"] != 3 {
		t.Errorf("prompt_tokens metric = %v, want 3", res.Metrics["prompt_tokens"])
	}
}

func TestRunnerScorerErrorDegradesGracefully(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued", RequestIDs: []string{"req_1"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	adapter := &fakeAdapter{name: "a", enabled: true, output: "ok",
		usage: domain.TokenUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{"req_1": testPayload()}},
		Logs:     &fakeLogs{entries: map[string]*domain.RequestLog{"req_1": {RequestID: "req_1", Provider: "a", RequestedModel: "m"}}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{"a": adapter}},
		Scorer:   errScorer{err: errors.New("scorer down")},
		References: &fakeRefs{refs: map[string]string{
			"req_1": "ok",
		}},
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	// A scorer outage must not lose the provider output or fail the job.
	if run.Status != "completed" {
		t.Fatalf("status = %q, want completed", run.Status)
	}
	if len(store.results) != 1 {
		t.Fatalf("results = %d, want 1", len(store.results))
	}
	res := store.results[0]
	if res.Score != 0 {
		t.Errorf("score = %v, want degraded 0", res.Score)
	}
	if res.Metrics["total_tokens"] != 2 {
		t.Errorf("total_tokens metric = %v, want 2 (usage preserved)", res.Metrics["total_tokens"])
	}
	if res.OutputPreview != "ok" {
		t.Errorf("preview = %q, want the provider output", res.OutputPreview)
	}
}

func TestRunnerPublishesCompletionEvent(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", TenantID: "t1", Status: "queued", RequestIDs: []string{"req_1"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	adapter := &fakeAdapter{name: "a", enabled: true, output: "ok"}
	pub := &fakePublisher{}
	r := NewRunner(RunnerDeps{
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{"req_1": testPayload()}},
		Logs:     &fakeLogs{entries: map[string]*domain.RequestLog{"req_1": {RequestID: "req_1", Provider: "a", RequestedModel: "m"}}},
		Store:    store,
		Adapters: &fakeAdapters{byName: map[string]*fakeAdapter{"a": adapter}},
		Events:   pub,
	})
	run, err := r.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	if len(pub.subjects) != 1 || pub.subjects[0] != SubjectReplayCompleted {
		t.Fatalf("published subjects = %v, want one %q event", pub.subjects, SubjectReplayCompleted)
	}
	evt, ok := pub.payloads[0].(CompletionEvent)
	if !ok {
		t.Fatalf("payload type = %T, want CompletionEvent", pub.payloads[0])
	}
	if evt.JobID != "job1" || evt.RunID != run.ID || evt.TenantID != "t1" {
		t.Errorf("event identity = %+v, want job1/%s/t1", evt, run.ID)
	}
	if evt.Status != "completed" || evt.Succeeded != 1 || evt.Total != 1 {
		t.Errorf("event = %+v, want completed 1/1", evt)
	}
}
