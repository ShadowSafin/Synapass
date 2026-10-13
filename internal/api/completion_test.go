package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// These tests cover the client-visible half of the truncation fix: an answer
// that was cut short must say so in the response, and a complete answer must not
// gain any new keys.
//
// Before the fix a truncated response was indistinguishable from a short one,
// which is why the bug survived: the client had no way to report what it saw.

// completionBlock extracts the completion metadata from a decoded response.
func completionBlock(t *testing.T, body []byte) map[string]any {
	t.Helper()

	var envelope struct {
		Synapass struct {
			Completion map[string]any `json:"completion"`
		} `json:"synapass"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return envelope.Synapass.Completion
}

// TestCompleteAnswerCarriesNoCompletionBlock keeps the additive contract: a
// normal response must look exactly as it did before.
func TestCompleteAnswerCarriesNoCompletionBlock(t *testing.T) {
	h := newHarness(t, harnessOptions{})

	resp := h.do(t, "POST", "/v1/chat/completions", testToken, chatBody(""), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)

	if strings.Contains(body, `"completion"`) {
		t.Errorf("a finished answer must not report a completion block: %s", body)
	}
}

// TestTruncatedAnswerIsReported is the regression test for the silent failure.
func TestTruncatedAnswerIsReported(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.primary.lengthFinish = true

	resp := h.do(t, "POST", "/v1/chat/completions", testToken, chatBody(""), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, readBody(t, resp))
	}

	completion := completionBlock(t, []byte(readBody(t, resp)))
	if completion == nil {
		t.Fatal("a response with finish_reason=length must report a completion block")
	}
	if completion["truncated"] != true {
		t.Errorf("truncated = %v, want true", completion["truncated"])
	}
	if completion["reason"] != "max_tokens" {
		t.Errorf("reason = %v, want max_tokens", completion["reason"])
	}
	if completion["finish_reason"] != "length" {
		t.Errorf("finish_reason = %v, want the provider's length", completion["finish_reason"])
	}
}

// TestSilentClientSeesNoAppliedTokens documents that the gateway did not invent
// a ceiling: applied_tokens is zero when nothing was capped upstream.
func TestSilentClientSeesNoAppliedTokens(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.primary.lengthFinish = true

	resp := h.do(t, "POST", "/v1/chat/completions", testToken, chatBody(""), nil)
	completion := completionBlock(t, []byte(readBody(t, resp)))
	if completion == nil {
		t.Fatal("expected a completion block for a truncated answer")
	}
	if _, present := completion["applied_tokens"]; present {
		t.Errorf("applied_tokens = %v, want it absent: the client set no limit",
			completion["applied_tokens"])
	}
	if _, present := completion["requested_tokens"]; present {
		t.Errorf("requested_tokens = %v, want it absent for a silent client",
			completion["requested_tokens"])
	}
}

// TestExplicitLimitIsEchoed confirms a client-set ceiling is reported, so a
// caller can see which limit stopped its answer.
func TestExplicitLimitIsEchoed(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.primary.lengthFinish = true

	resp := h.do(t, "POST", "/v1/chat/completions", testToken, chatBody(`"max_tokens":256`), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}

	completion := completionBlock(t, []byte(readBody(t, resp)))
	if completion == nil {
		t.Fatal("expected a completion block for a truncated answer")
	}
	if completion["requested_tokens"] != float64(256) {
		t.Errorf("requested_tokens = %v, want 256", completion["requested_tokens"])
	}
	if completion["applied_tokens"] != float64(256) {
		t.Errorf("applied_tokens = %v, want 256", completion["applied_tokens"])
	}
}

// TestStreamedTruncationReportsOnTheUsageFrame checks the streaming path, where
// the metadata rides on the terminal usage frame rather than a JSON body.
func TestStreamedTruncationReportsOnTheUsageFrame(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.primary.lengthFinish = true

	resp := h.do(t, "POST", "/v1/chat/completions", testToken,
		chatBody(`"stream":true,"stream_options":{"include_usage":true}`), nil)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	body := readBody(t, resp)

	if !strings.Contains(body, `"completion"`) {
		t.Errorf("a truncated stream must report a completion block: %s", body)
	}
	if !strings.Contains(body, `"truncated":true`) {
		t.Errorf("expected truncated:true on the usage frame: %s", body)
	}
	if !strings.Contains(body, `"finish_reason":"length"`) {
		t.Errorf("expected the provider finish reason on the stream: %s", body)
	}
}

// TestDebugMetadataAlsoCarriesCompletion keeps the admin view consistent with
// the public one; an operator debugging a truncation needs the same facts.
func TestDebugMetadataAlsoCarriesCompletion(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	h.primary.lengthFinish = true

	resp := h.do(t, "POST", "/v1/chat/completions", testToken, chatBody(""),
		map[string]string{"X-Synapass-Debug": "true"})
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, readBody(t, resp))
	}

	completion := completionBlock(t, []byte(readBody(t, resp)))
	if completion == nil || completion["truncated"] != true {
		t.Errorf("debug metadata must report the truncation, got %v", completion)
	}
}
