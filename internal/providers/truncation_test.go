package providers

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

// These tests cover the provider side of the truncation bug: the field that
// caps generation was being populated from a router default even when the client
// asked for no limit, so long answers stopped early and nothing reported it.

// TestOpenAIOmitsMaxTokensWhenNoneRequested pins that a silent client produces
// no max_tokens on the wire, letting the provider's own default stand.
func TestOpenAIOmitsMaxTokensWhenNoneRequested(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, openAISuccessBody)
	})
	adapter := adapterFor(t, providerFor("openai", domain.ProviderOpenAI, "https://api.openai.com/v1"), transport)

	req := chatRequest()
	req.MaxTokens = 0 // the router found no client preference

	if _, err := adapter.ChatCompletion(context.Background(), req); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	body := transport.lastBody(t)
	if value, present := body["max_tokens"]; present {
		t.Errorf("max_tokens = %v, want the field absent so the provider decides", value)
	}
	if value, present := body["max_completion_tokens"]; present {
		t.Errorf("max_completion_tokens = %v, want absent", value)
	}
}

// TestOllamaOmitsNumPredictWhenNoneRequested is the same rule for the local
// runtime, whose num_predict field is Ollama's equivalent cap.
func TestOllamaOmitsNumPredictWhenNoneRequested(t *testing.T) {
	ollama := providerFor("ollama", domain.ProviderOllama, "http://localhost:11434")
	ollama.AuthStyle = ""
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, `{"message":{"role":"assistant","content":"hi"},"done":true}`)
	})
	adapter := adapterFor(t, ollama, transport)

	req := chatRequest()
	req.MaxTokens = 0

	if _, err := adapter.ChatCompletion(context.Background(), req); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	body := transport.lastBody(t)
	options, ok := body["options"].(map[string]any)
	if !ok {
		t.Fatalf("expected an options block, got %v", body["options"])
	}
	if value, present := options["num_predict"]; present {
		t.Errorf("num_predict = %v, want absent so generation is not capped", value)
	}
}

// TestAnthropicUsesGenerousDefaultWhenNoneRequested covers the mandatory field.
//
// Anthropic rejects a request without max_tokens, so the adapter must supply
// something. It must not be a small constant: that constant was the reason long
// answers stopped mid-sentence on this provider.
func TestAnthropicUsesGenerousDefaultWhenNoneRequested(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, anthropicSuccessBody)
	})
	adapter := adapterFor(t, providerFor("anthropic", domain.ProviderAnthropic, "https://api.anthropic.com/v1"), transport)

	req := chatRequest()
	req.MaxTokens = 0

	if _, err := adapter.ChatCompletion(context.Background(), req); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	body := transport.lastBody(t)
	maxTokens, ok := body["max_tokens"].(float64)
	if !ok {
		t.Fatalf("max_tokens must be present for Anthropic, got %v", body["max_tokens"])
	}
	if maxTokens < 16384 {
		t.Errorf("max_tokens = %v, want a generous default that cannot truncate a long answer", maxTokens)
	}
	if int(maxTokens) != defaultAnthropicMaxTokens {
		t.Errorf("max_tokens = %v, want defaultAnthropicMaxTokens = %d", maxTokens, defaultAnthropicMaxTokens)
	}
}

// TestAnthropicHonoursAnExplicitMaxTokens confirms the default only applies
// when the router had nothing to send.
func TestAnthropicHonoursAnExplicitMaxTokens(t *testing.T) {
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, anthropicSuccessBody)
	})
	adapter := adapterFor(t, providerFor("anthropic", domain.ProviderAnthropic, "https://api.anthropic.com/v1"), transport)

	if _, err := adapter.ChatCompletion(context.Background(), chatRequest()); err != nil {
		t.Fatalf("ChatCompletion: %v", err)
	}

	body := transport.lastBody(t)
	if body["max_tokens"] != float64(128) {
		t.Errorf("max_tokens = %v, want the client's 128", body["max_tokens"])
	}
}

// TestTransportHasNoWholeResponseHeaderDeadline is the regression test for the
// buffered-request failure: ResponseHeaderTimeout used to equal the first-token
// budget, but a buffered provider only sends headers once the entire answer
// exists, so every long completion was aborted before a byte of body arrived.
func TestTransportHasNoWholeResponseHeaderDeadline(t *testing.T) {
	client := NewHTTPClient(domain.DefaultTimeoutPolicy())
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", client.Transport)
	}
	if transport.ResponseHeaderTimeout != 0 {
		t.Errorf("ResponseHeaderTimeout = %s, want 0: it must never bound a buffered generation",
			transport.ResponseHeaderTimeout)
	}
	if client.Timeout != 0 {
		t.Errorf("Client.Timeout = %s, want 0: it would also cut a streaming body", client.Timeout)
	}
	if transport.DialContext == nil {
		t.Error("connect timeout must still be enforced through the dialer")
	}
}

// TestBufferedBodyOverLimitIsAnError confirms a large response is rejected
// explicitly rather than parsed as a short, silently truncated body.
func TestBufferedBodyOverLimitIsAnError(t *testing.T) {
	oversized := strings.Repeat("x", 2048)
	transport := newStubTransport(func(*http.Request) *http.Response {
		return jsonResponse(http.StatusOK, oversized)
	})
	adapter := adapterFor(t, providerFor("openai", domain.ProviderOpenAI, "https://api.openai.com/v1"), transport)

	if _, err := adapter.ChatCompletion(context.Background(), chatRequest()); err == nil {
		t.Fatal("an over-limit body must produce an error, not a partial answer")
	}
}

// TestStreamingLineLimitIsExplicit confirms the SSE scanner fails loudly rather
// than silently dropping the tail of a very long line, which would look exactly
// like a model that stopped mid-sentence.
func TestStreamingLineLimitIsExplicit(t *testing.T) {
	if maxStreamLineBytes < 1<<20 {
		t.Errorf("maxStreamLineBytes = %d, want at least 1 MiB", maxStreamLineBytes)
	}

	reader := newSSEReader(strings.NewReader("data: " + strings.Repeat("y", maxStreamLineBytes+16) + "\n\n"))
	_, ok, err := reader.Next()
	if err == nil {
		t.Fatal("an over-long SSE line must error instead of being silently cut")
	}
	if !strings.Contains(err.Error(), "token too long") && !errors.Is(err, bufio.ErrTooLong) {
		t.Errorf("error = %v, want an explicit scanner limit failure", err)
	}
	if ok {
		t.Error("no event should be reported for a line that could not be read")
	}
}

// TestSSEReaderDeliversALargeAnswerIntact guards the opposite failure: a long
// but legal stream must arrive whole.
func TestSSEReaderDeliversALargeAnswerIntact(t *testing.T) {
	var buf strings.Builder
	buf.WriteString("data: {\"delta\":\"start\"}\n\n")
	for i := 0; i < 2000; i++ {
		buf.WriteString("data: {\"delta\":\"a moderately long chunk of text " + strconv.Itoa(i) + "\"}\n\n")
	}
	buf.WriteString("data: [DONE]\n\n")

	reader := newSSEReader(strings.NewReader(buf.String()))
	var frames int
	var done bool
	for {
		ev, ok, err := reader.Next()
		if err != nil {
			t.Fatalf("a long stream must not error: %v", err)
		}
		if !ok {
			break
		}
		if ev.done() {
			done = true
			break
		}
		frames++
	}
	if !done {
		t.Error("the terminating [DONE] frame never arrived; the stream was cut short")
	}
	if frames != 2001 {
		t.Errorf("frames = %d, want 2001", frames)
	}
}
