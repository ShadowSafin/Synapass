package api

import (
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// streamResponseStorable decides what an assembled stream teaches the cache.
// The decisive signal is Execute success (adapters only return success on a
// clean end of stream); a missing finish reason means the provider closed a
// healthy stream without one, not an incomplete response. Only empty,
// over-limit, tool-call and explicitly-failed responses are rejected.

func streamResp(content string, finish *domain.FinishReason) *providers.Response {
	msg := &domain.ChatMessage{Role: domain.RoleAssistant, Content: domain.NewTextContent(content)}
	return &providers.Response{
		ID: "chatcmpl-1", Model: "m", Created: 1700000000,
		Choices: []domain.Choice{{Index: 0, Message: msg, FinishReason: finish}},
		Usage:   domain.TokenUsage{PromptTokens: 3, CompletionTokens: 5},
	}
}

func finishPtr(r domain.FinishReason) *domain.FinishReason { return &r }

func TestStreamStoresCleanCompletion(t *testing.T) {
	if !streamResponseStorable(streamResp("hello", finishPtr(domain.FinishStop)), 1<<20) {
		t.Fatalf("clean stopped stream must be storable")
	}
	if !streamResponseStorable(streamResp("cut", finishPtr(domain.FinishLength)), 1<<20) {
		t.Fatalf("length-finished stream must be storable (max tokens are in the key)")
	}
	// Providers that close a healthy stream without a finish reason still
	// produced a complete response: Execute succeeded.
	if !streamResponseStorable(streamResp("hello", nil), 1<<20) {
		t.Fatalf("stream without a finish reason must be storable after clean EOF")
	}
}

func TestStreamRejectsToolCalls(t *testing.T) {
	resp := streamResp("let me check", finishPtr(domain.FinishToolCalls))
	resp.Choices[0].Message.ToolCalls = []domain.ToolCall{
		{Function: domain.FunctionCall{Name: "echo", Arguments: `{"a":1}`}},
	}
	if streamResponseStorable(resp, 1<<20) {
		t.Fatalf("tool-call stream must never be stored")
	}
}

func TestStreamRejectsIncomplete(t *testing.T) {
	if streamResponseStorable(streamResp("", finishPtr(domain.FinishStop)), 1<<20) {
		t.Fatalf("stream without content must not be stored")
	}
	if streamResponseStorable(nil, 1<<20) {
		t.Fatalf("nil response must not be stored")
	}
	if streamResponseStorable(&providers.Response{}, 1<<20) {
		t.Fatalf("empty response must not be stored")
	}
}

func TestStreamRejectsErrorFinish(t *testing.T) {
	if streamResponseStorable(streamResp("oops", finishPtr(domain.FinishError)), 1<<20) {
		t.Fatalf("error-finished stream must not be stored")
	}
	if streamResponseStorable(streamResp("blocked", finishPtr(domain.FinishContentFilter)), 1<<20) {
		t.Fatalf("filtered stream must not be stored")
	}
}

func TestStreamRejectsOverLimit(t *testing.T) {
	if streamResponseStorable(streamResp("way too much text", nil), 8) {
		t.Fatalf("over-limit stream must not be stored")
	}
	if !streamResponseStorable(streamResp("way too much text", nil), 0) {
		t.Fatalf("zero limit must mean unbounded")
	}
}
