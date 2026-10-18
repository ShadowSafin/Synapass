package providers

import (
	"net/http"
	"strings"
	"testing"
)

func TestSanitizeProviderMessageRedactsCredentials(t *testing.T) {
	cases := []string{
		"Incorrect API key provided: sk-live-abcdefghijklmnop1234",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature",
		"x-api-key: abcdef0123456789abcdef",
		`{"error":"bad token","api_key":"secret-value-here"}`,
	}
	for _, message := range cases {
		got := sanitizeProviderMessage(message)
		if !strings.Contains(got, "[redacted]") {
			t.Errorf("sanitizeProviderMessage(%q) = %q, want redaction", message, got)
		}
		for _, secret := range []string{"abcdefghijklmnop1234", "eyJhbGciOiJIUzI1NiJ9", "abcdef0123456789abcdef", "secret-value-here"} {
			if strings.Contains(got, secret) {
				t.Errorf("sanitizeProviderMessage(%q) leaked %q", message, secret)
			}
		}
	}
}

func TestSanitizeProviderMessageKeepsOrdinaryText(t *testing.T) {
	const message = "the model gpt-4o does not support tool calls"
	if got := sanitizeProviderMessage(message); got != message {
		t.Fatalf("ordinary text was altered: %q", got)
	}
}

func TestNormalizeHTTPErrorRedactsAnEchoedKey(t *testing.T) {
	body := []byte(`{"error":{"message":"Incorrect API key provided: sk-live-abcdefghijklmnop1234","type":"invalid_request_error"}}`)
	err := NormalizeHTTPError("openai", "gpt-4o", 1, http.StatusUnauthorized, body, http.Header{})

	if strings.Contains(err.Message, "abcdefghijklmnop1234") {
		t.Fatalf("error message leaked the key: %q", err.Message)
	}
	if !strings.Contains(err.Message, "[redacted]") {
		t.Fatalf("error message = %q, want a redaction marker", err.Message)
	}
}
