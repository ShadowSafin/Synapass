package replay

import (
	"context"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

func TestRebuildRequestPrefersPayloadMessages(t *testing.T) {
	entry := &domain.RequestLog{RequestID: "req_1", RequestedModel: "asked"}
	payload := &domain.RequestPayload{
		RequestID: "req_1",
		Model:     "captured",
		Messages:  []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("real prompt")}},
	}
	req := rebuildRequest(entry, payload)
	if len(req.Messages) != 1 || req.Messages[0].Text() != "real prompt" {
		t.Fatalf("messages = %+v, want the captured payload", req.Messages)
	}
	if req.Model != "asked" {
		t.Errorf("model = %q, want the requested model", req.Model)
	}
}

func TestRebuildRequestFallsBackToStub(t *testing.T) {
	for name, payload := range map[string]*domain.RequestPayload{
		"nil payload":   nil,
		"empty payload": {RequestID: "req_1"},
	} {
		entry := &domain.RequestLog{RequestID: "req_1", RequestedModel: "m"}
		req := rebuildRequest(entry, payload)
		if len(req.Messages) != 1 || req.Messages[0].Text() != "[replay:req_1]" {
			t.Errorf("%s: messages = %+v, want the stub fallback", name, req.Messages)
		}
	}
}

func TestServiceRunScoresResults(t *testing.T) {
	store := newFakeStore()
	job := &domain.ReplayJob{ID: "job1", Status: "queued", RequestIDs: []string{"req_1"}, MaxRequests: 10}
	store.jobs[job.ID] = job
	var gotPrompt domain.ChatCompletionRequest
	svc := NewWithDeps(ServiceDeps{
		Requests: &fakeLogs{entries: map[string]*domain.RequestLog{
			"req_1": {RequestID: "req_1", RequestedModel: "m"},
		}},
		Exec: func(_ context.Context, prompt domain.ChatCompletionRequest, _ domain.RouteTarget) (string, int64, float64, error) {
			gotPrompt = prompt
			return "hello", 7, 0.001, nil
		},
		Store: store,
		Payloads: &fakePayloads{entries: map[string]*domain.RequestPayload{
			"req_1": {RequestID: "req_1", Model: "m", Messages: []domain.ChatMessage{
				{Role: domain.RoleUser, Content: domain.NewTextContent("real prompt")},
			}},
		}},
		Scorer: EvalScorer{},
		References: &fakeRefs{refs: map[string]string{
			"req_1": "hello",
		}},
		Events: &fakePublisher{},
	})
	run, err := svc.Run(context.Background(), "job1")
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" {
		t.Fatalf("status = %q, want completed (error %q)", run.Status, run.Error)
	}
	// The executor must see the captured prompt, not the stub.
	if len(gotPrompt.Messages) != 1 || gotPrompt.Messages[0].Text() != "real prompt" {
		t.Fatalf("executed prompt = %+v, want the captured messages", gotPrompt.Messages)
	}
	if len(store.results) != 1 {
		t.Fatalf("results = %d, want 1", len(store.results))
	}
	res := store.results[0]
	if diff := 1 - res.Score; diff < 0 || diff > 1e-9 {
		t.Errorf("score = %v, want 1 for an exact-match output", res.Score)
	}
	if res.Metrics["token_f1"] != 1 {
		t.Errorf("token_f1 = %v, want 1", res.Metrics["token_f1"])
	}
}
