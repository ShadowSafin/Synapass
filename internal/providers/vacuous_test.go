package providers

import (
	"context"
	"net/http"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Vacuous tool responses
//
// A tool-carrying request answered with nothing (no text, no calls, no
// refusal) must fail as upstream rather than serve an empty 200 that stalls
// every agent loop.
// ---------------------------------------------------------------------------

func toolRequest() *Request {
	req := chatRequest()
	req.Params.Tools = []domain.Tool{{
		Type:     "function",
		Function: domain.FunctionDefinition{Name: "read", Parameters: []byte(`{"type":"object"}`)},
	}}
	return req
}

const openAIVacuousBody = `{
  "id": "chatcmpl-empty",
  "object": "chat.completion",
  "created": 1750000000,
  "model": "gemini-3.8-flash",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": ""},
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 16, "completion_tokens": 124, "total_tokens": 140}
}`

func TestVacuousToolResponseMatrix(t *testing.T) {
	withTools := &domain.ChatCompletionRequest{
		Tools: []domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "read"}}},
	}
	withoutTools := &domain.ChatCompletionRequest{}
	msg := func(text, refusal string, calls int) *domain.ChatMessage {
		m := &domain.ChatMessage{Role: domain.RoleAssistant, Refusal: refusal}
		if text != "" {
			m.Content = domain.NewTextContent(text)
		}
		for i := 0; i < calls; i++ {
			m.ToolCalls = append(m.ToolCalls, domain.ToolCall{ID: "c", Type: "function"})
		}
		return m
	}
	cases := []struct {
		name   string
		params *domain.ChatCompletionRequest
		resp   *Response
		want   bool
	}{
		{"tools with empty answer", withTools, &Response{Choices: []domain.Choice{{Message: msg("", "", 0)}}}, true},
		{"tools with nil message", withTools, &Response{Choices: []domain.Choice{{}}}, true},
		{"tools with no choices", withTools, &Response{}, true},
		{"tools with text", withTools, &Response{Choices: []domain.Choice{{Message: msg("hi", "", 0)}}}, false},
		{"tools with calls", withTools, &Response{Choices: []domain.Choice{{Message: msg("", "", 1)}}}, false},
		{"tools with refusal", withTools, &Response{Choices: []domain.Choice{{Message: msg("", "no", 0)}}}, false},
		{"no tools with empty answer", withoutTools, &Response{Choices: []domain.Choice{{Message: msg("", "", 0)}}}, false},
		{"nil params", nil, &Response{Choices: []domain.Choice{{Message: msg("", "", 0)}}}, false},
	}
	for _, tc := range cases {
		if got := VacuousToolResponse(tc.params, tc.resp); got != tc.want {
			t.Errorf("%s: VacuousToolResponse = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestOpenAIAdapterRejectsVacuousToolResponse(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, openAIVacuousBody)
	})
	adapter := adapterFor(t, providerFor("compat", domain.ProviderOpenAICompatible, "http://localhost:8000/v1"), transport)

	_, err := adapter.ChatCompletion(context.Background(), toolRequest())
	if err == nil {
		t.Fatal("an empty answer to a tool request must fail, not serve an empty 200")
	}
	derr := domain.AsError(err)
	if derr.Code != domain.ErrCodeUpstream {
		t.Errorf("code = %q, want upstream_error so the executor fails over", derr.Code)
	}
	if derr.Provider == "" {
		t.Error("the error must name the provider for attribution and health")
	}
}

func TestOpenAIAdapterStillServesEmptyPlainChat(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, openAIVacuousBody)
	})
	adapter := adapterFor(t, providerFor("compat", domain.ProviderOpenAICompatible, "http://localhost:8000/v1"), transport)

	// No tools on the request: existing plain-chat behaviour is untouched.
	response, err := adapter.ChatCompletion(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("an empty plain-chat answer must still succeed: %v", err)
	}
	if response.Content() != "" {
		t.Errorf("content = %q, want empty", response.Content())
	}
}

const openAIVacuousStreamBody = `data: {"id":"chatcmpl-e2","object":"chat.completion.chunk","created":1750000000,"model":"gemini-3.8-flash","choices":[{"index":0,"delta":{"role":"assistant","content":""}}]}

data: {"id":"chatcmpl-e2","object":"chat.completion.chunk","created":1750000000,"model":"gemini-3.8-flash","choices":[{"index":0,"finish_reason":"stop"}]}

data: [DONE]

`

func TestOpenAIAdapterRejectsVacuousToolStream(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return sseResponse(openAIVacuousStreamBody)
	})
	adapter := adapterFor(t, providerFor("compat", domain.ProviderOpenAICompatible, "http://localhost:8000/v1"), transport)

	_, err := adapter.ChatCompletionStream(context.Background(), toolRequest(), func(Chunk) error { return nil })
	if err == nil {
		t.Fatal("an empty tool stream must surface an error, not a clean EOF")
	}
	if code := domain.AsError(err).Code; code != domain.ErrCodeUpstream {
		t.Errorf("code = %q, want upstream_error", code)
	}
}
