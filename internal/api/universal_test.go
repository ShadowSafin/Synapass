package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Universal inference endpoint
//
// These tests pin the public contract from the caller's side: flexible input,
// a clean default response, opt-in routing internals, honest fallback, strict
// auth and policy denials, and a complete SSE stream. They run against the
// same stubbed pipeline as the rest of the suite: no database, no network.
// ---------------------------------------------------------------------------

func TestNormalizePromptShorthand(t *testing.T) {
	req := domain.ChatCompletionRequest{Model: "m", Prompt: "hello there"}
	if err := req.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != domain.RoleUser {
		t.Fatalf("messages = %+v", req.Messages)
	}
	if got := req.Messages[0].Content.PlainText(); got != "hello there" {
		t.Errorf("content = %q", got)
	}
	if req.Prompt != "" {
		t.Error("the shorthand must be consumed so downstream sees one shape")
	}
	if err := req.Validate(); err != nil {
		t.Errorf("a normalized prompt request must validate: %v", err)
	}
}

func TestNormalizeRejectsPromptPlusMessages(t *testing.T) {
	req := domain.ChatCompletionRequest{
		Model:    "m",
		Prompt:   "hi",
		Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("hi")}},
	}
	if err := req.Normalize(); err == nil {
		t.Error("prompt and messages together must be rejected, not silently merged")
	}
}

func TestNormalizeSystemShorthand(t *testing.T) {
	req := domain.ChatCompletionRequest{Model: "m", Prompt: "hi", System: "be brief"}
	if err := req.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != domain.RoleSystem {
		t.Fatalf("system must lead: %+v", req.Messages)
	}
	if got := req.Messages[0].Content.PlainText(); got != "be brief" {
		t.Errorf("system content = %q", got)
	}
}

func TestChatCompletionsAcceptsPromptShorthand(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	body := `{"model":"` + testModel + `","prompt":"hello via shorthand","temperature":0.5}`
	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	var decoded struct {
		Choices []domain.Choice `json:"choices"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := decoded.Choices[0].Message.Text(); got != "hello from the primary" {
		t.Errorf("content = %q", got)
	}
}

func TestPublicMetadataHidesRoutingInternals(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	raw := readBody(t, resp)

	var decoded struct {
		Core *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Core == nil || decoded.Core.RequestID == "" {
		t.Fatal("the default response must carry a request id")
	}
	if decoded.Core.Provider != "openai" || decoded.Core.RoutedModel == "" {
		t.Errorf("stable attribution must survive: %+v", decoded.Core)
	}
	// Routing internals must not leak to normal clients.
	for name, value := range map[string]string{
		"trace_id":     decoded.Core.TraceID,
		"policy_id":    decoded.Core.PolicyID,
		"policy_name":  decoded.Core.PolicyName,
		"strategy":     decoded.Core.Strategy,
		"task":         decoded.Core.Task,
		"shaping":      decoded.Core.Shaping,
		"route_reason": decoded.Core.RouteReason,
	} {
		if value != "" {
			t.Errorf("%s leaked into the default response: %q", name, value)
		}
	}
	if decoded.Core.Attempts != 0 {
		t.Errorf("attempts leaked into the default response: %d", decoded.Core.Attempts)
	}
}

func TestDebugHeaderRevealsRoutingInternals(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"X-Synapass-Debug": "true"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	var decoded struct {
		Core *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Core == nil {
		t.Fatal("debug mode must keep the metadata block")
	}
	if decoded.Core.PolicyName != "default" || decoded.Core.Strategy != "priority" {
		t.Errorf("debug metadata = %+v", decoded.Core)
	}
	if decoded.Core.RouteReason == "" || decoded.Core.Task == "" {
		t.Errorf("debug metadata must explain the route: %+v", decoded.Core)
	}
}

func TestFallbackReturnsCleanAnswer(t *testing.T) {
	h := newHarness(t, harnessOptions{failPrimary: true})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a fallback must still be a 200, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var decoded struct {
		Choices []domain.Choice          `json:"choices"`
		Core    *domain.ResponseMetadata `json:"synapass"`
		Error   *domain.ErrorBody        `json:"error,omitempty"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Error != nil {
		t.Errorf("a recovered fallback must not carry an error: %+v", decoded.Error)
	}
	if got := decoded.Choices[0].Message.Text(); got != "hello from the secondary" {
		t.Errorf("content = %q, want the fallback answer", got)
	}
	if decoded.Core == nil || decoded.Core.Provider != "anthropic" || !decoded.Core.FallbackUsed {
		t.Errorf("fallback attribution = %+v", decoded.Core)
	}
}

func TestAuthFailureEnvelope(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", "bad-token", chatBody(""), nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	var decoded struct {
		Error domain.ErrorBody         `json:"error"`
		Core  *domain.ResponseMetadata `json:"synapass"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Error.Code != string(domain.ErrCodeAuthentication) {
		t.Errorf("code = %q", decoded.Error.Code)
	}
	if decoded.Core == nil || decoded.Core.RequestID == "" {
		t.Error("even a rejection must carry a request id for support correlation")
	}
}

func TestPolicyDenyReturnsCleanDenial(t *testing.T) {
	h := newHarness(t, harnessOptions{denyPolicy: "this tenant may not use chat models"})

	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, chatBody(""), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", resp.StatusCode, readBody(t, resp))
	}
	var decoded struct {
		Error domain.ErrorBody `json:"error"`
	}
	if err := json.Unmarshal([]byte(readBody(t, resp)), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Error.Code != string(domain.ErrCodePermission) {
		t.Errorf("code = %q", decoded.Error.Code)
	}
	if !strings.Contains(decoded.Error.Message, "may not use chat models") {
		t.Errorf("the denial must explain itself: %q", decoded.Error.Message)
	}
	if h.primary.callCount() != 0 || h.secondary.callCount() != 0 {
		t.Error("a denied request must never reach a provider")
	}
}

func TestStreamingCompletesWithUsageFrame(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	body := `{"model":"` + testModel + `","messages":[{"role":"user","content":"hi"}],"stream":true,"stream_options":{"include_usage":true}}`
	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}

	frames := strings.Split(strings.TrimSpace(readBody(t, resp)), "\n\n")
	if frames[len(frames)-1] != "data: [DONE]" {
		t.Fatalf("stream must end with [DONE], got %q", frames[len(frames)-1])
	}
	var sawDelta, sawUsage bool
	for _, frame := range frames[:len(frames)-1] {
		payload := strings.TrimPrefix(frame, "data: ")
		var chunk struct {
			Object  string `json:"object"`
			Choices []struct {
				Delta *domain.ChatMessage `json:"delta"`
			} `json:"choices"`
			Usage *domain.TokenUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("frame decode: %v (%q)", err, payload)
		}
		if chunk.Object != domain.ObjectChatCompletionChunk {
			t.Errorf("object = %q", chunk.Object)
		}
		for _, choice := range chunk.Choices {
			if choice.Delta != nil && choice.Delta.Text() == "hello from the primary" {
				sawDelta = true
			}
		}
		if chunk.Usage != nil && chunk.Usage.TotalTokens == 15 {
			sawUsage = true
		}
	}
	if !sawDelta {
		t.Error("no content frame carried the answer")
	}
	if !sawUsage {
		t.Error("no terminal usage frame was sent despite stream_options.include_usage")
	}
}

func TestUnknownFieldTolerated(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	// The inference surface is lenient by design: OpenAI-compatible
	// clients send extension fields (cache_control, store, service_tier,
	// future additions) that Synapass accepts and ignores. A misspelled
	// sampling field is therefore ignored rather than rejected; value
	// validation (ranges, roles, empty content) still 400s.
	body := `{"model":"` + testModel + `","messages":[{"role":"user","content":"hi"}],"temprature":0.5}`
	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a tolerated unknown field: %s", resp.StatusCode, readBody(t, resp))
	}
}

func TestSamplingControlsValidation(t *testing.T) {
	valid := domain.ChatCompletionRequest{
		Model:             "m",
		Messages:          []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("hi")}},
		TopK:              intPtr(40),
		MinP:              floatPtr(0.05),
		RepetitionPenalty: floatPtr(1.1),
	}
	if err := valid.Validate(); err != nil {
		t.Errorf("valid sampling controls rejected: %v", err)
	}

	badTopK := valid
	badTopK.TopK = intPtr(0)
	if err := badTopK.Validate(); err == nil {
		t.Error("top_k 0 must be rejected")
	}
	badMinP := valid
	badMinP.MinP = floatPtr(2)
	if err := badMinP.Validate(); err == nil {
		t.Error("min_p above 1 must be rejected")
	}
	badRep := valid
	badRep.RepetitionPenalty = floatPtr(-1)
	if err := badRep.Validate(); err == nil {
		t.Error("a negative repetition_penalty must be rejected")
	}
}

func TestChatCompletionsAcceptsTopK(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	body := `{"model":"` + testModel + `","messages":[{"role":"user","content":"hi"}],"top_k":40,"min_p":0.05,"repetition_penalty":1.1}`
	resp := h.do(t, http.MethodPost, "/v1/chat/completions", testToken, body, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
}

func intPtr(v int) *int { return &v }

func floatPtr(v float64) *float64 { return &v }
