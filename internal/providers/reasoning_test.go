package providers

import (
	"context"
	"net/http"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ---------------------------------------------------------------------------
// Thinking traces
//
// Providers that emit thinking (reasoning_content on OpenAI-compatible
// servers, thinking blocks on Anthropic) have it captured into
// ChatMessage.Reasoning, never mixed into the answer text. Clients render
// reasoning_content as a separate trace; surfacing it as content would read
// as part of the answer.
// ---------------------------------------------------------------------------

const openAIReasoningBody = `{
  "id": "chatcmpl-r1",
  "object": "chat.completion",
  "created": 1750000000,
  "model": "deepseek-reasoner",
  "choices": [{
    "index": 0,
    "message": {"role": "assistant", "content": "42", "reasoning_content": "1+1 is 2, times 21 is 42."},
    "finish_reason": "stop"
  }],
  "usage": {"prompt_tokens": 8, "completion_tokens": 30, "total_tokens": 38}
}`

func TestOpenAIAdapterCapturesReasoningContent(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, openAIReasoningBody)
	})
	adapter := adapterFor(t, providerFor("compat", domain.ProviderOpenAICompatible, "http://localhost:8000/v1"), transport)

	response, err := adapter.ChatCompletion(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	if got := response.Choices[0].Message.Reasoning; got != "1+1 is 2, times 21 is 42." {
		t.Errorf("reasoning = %q", got)
	}
	// The trace must not leak into the answer.
	if got := response.Content(); got != "42" {
		t.Errorf("content = %q, want the answer without the trace", got)
	}
}

func TestOpenAIAdapterToleratesAbsentReasoning(t *testing.T) {
	for name, body := range map[string]string{
		"missing field": openAISuccessBody,
		"null field": `{"id":"x","object":"chat.completion","created":1,"model":"m",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi","reasoning_content":null},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
	} {
		t.Run(name, func(t *testing.T) {
			transport := newStubTransport(func(*http.Request) *http.Response {
				return jsonResponse(http.StatusOK, body)
			})
			adapter := adapterFor(t, providerFor("compat", domain.ProviderOpenAICompatible, "http://localhost:8000/v1"), transport)
			response, err := adapter.ChatCompletion(context.Background(), chatRequest())
			if err != nil {
				t.Fatalf("ChatCompletion: %v", err)
			}
			if got := response.Choices[0].Message.Reasoning; got != "" {
				t.Errorf("reasoning = %q, want empty", got)
			}
		})
	}
}

const openAIReasoningStreamBody = `data: {"id":"chatcmpl-r2","object":"chat.completion.chunk","created":1750000000,"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"first thought. "}}]}

data: {"id":"chatcmpl-r2","object":"chat.completion.chunk","created":1750000000,"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"reasoning_content":"second thought."}}]}

data: {"id":"chatcmpl-r2","object":"chat.completion.chunk","created":1750000000,"model":"deepseek-reasoner","choices":[{"index":0,"delta":{"content":"done"}}]}

data: {"id":"chatcmpl-r2","object":"chat.completion.chunk","created":1750000000,"model":"deepseek-reasoner","choices":[{"index":0,"finish_reason":"stop"}]}

data: [DONE]

`

func TestOpenAIAdapterStreamsReasoningSeparately(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return sseResponse(openAIReasoningStreamBody)
	})
	adapter := adapterFor(t, providerFor("compat", domain.ProviderOpenAICompatible, "http://localhost:8000/v1"), transport)

	var delivered []Chunk
	response, err := adapter.ChatCompletionStream(context.Background(), chatRequest(), func(chunk Chunk) error {
		delivered = append(delivered, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	// Two thinking frames, one answer frame, one terminal frame.
	if len(delivered) != 4 {
		t.Fatalf("delivered %d chunks, want 4", len(delivered))
	}
	if got := delivered[0].Delta.Reasoning; got != "first thought. " {
		t.Errorf("first reasoning delta = %q", got)
	}
	if got := delivered[0].Delta.Text(); got != "" {
		t.Errorf("thinking frame must carry no answer text, got %q", got)
	}
	if got := delivered[2].Delta.Text(); got != "done" {
		t.Errorf("answer delta = %q", got)
	}
	// The assembled record carries the full trace for cache and logs.
	msg := response.Choices[0].Message
	if got := msg.Reasoning; got != "first thought. second thought." {
		t.Errorf("assembled reasoning = %q", got)
	}
	if got := msg.Text(); got != "done" {
		t.Errorf("assembled content = %q", got)
	}
}

const anthropicThinkingBody = `{
  "id": "msg-1",
  "type": "message",
  "role": "assistant",
  "model": "claude-sonnet-4-6",
  "content": [
    {"type": "thinking", "thinking": "The user asks 1+1.", "signature": "sig"},
    {"type": "redacted_thinking", "data": "ZW5jcnlwdGVk"},
    {"type": "text", "text": "2"}
  ],
  "stop_reason": "end_turn",
  "usage": {"input_tokens": 10, "output_tokens": 20}
}`

func TestAnthropicAdapterCapturesThinkingBlocks(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, anthropicThinkingBody)
	})
	adapter := adapterFor(t, anthropicProvider(), transport)

	response, err := adapter.ChatCompletion(context.Background(), chatRequest())
	if err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}
	msg := response.Choices[0].Message
	if got := msg.Reasoning; got != "The user asks 1+1." {
		t.Errorf("reasoning = %q", got)
	}
	if got := msg.Text(); got != "2" {
		t.Errorf("content = %q, want the answer without thinking or ciphertext", got)
	}
}

const anthropicThinkingStreamBody = `event: message_start
data: {"type":"message_start","message":{"id":"msg-2","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Thinking out"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":" loud."}}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"2"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":20}}

`

func TestAnthropicAdapterStreamsThinkingDeltas(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return sseResponse(anthropicThinkingStreamBody)
	})
	adapter := adapterFor(t, anthropicProvider(), transport)

	var delivered []Chunk
	response, err := adapter.ChatCompletionStream(context.Background(), chatRequest(), func(chunk Chunk) error {
		delivered = append(delivered, chunk)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatCompletionStream: %v", err)
	}
	var thinking, answer string
	for _, c := range delivered {
		thinking += c.Delta.Reasoning
		answer += c.Delta.Text()
	}
	if thinking != "Thinking out loud." {
		t.Errorf("streamed thinking = %q", thinking)
	}
	if answer != "2" {
		t.Errorf("streamed answer = %q", answer)
	}
	if got := response.Choices[0].Message.Reasoning; got != "Thinking out loud." {
		t.Errorf("assembled reasoning = %q", got)
	}
}
