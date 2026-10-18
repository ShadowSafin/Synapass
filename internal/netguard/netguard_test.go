package netguard

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestValidateHTTPURLRejectsUnsafeForms(t *testing.T) {
	bad := []string{
		"",
		"ftp://example.com/x",
		"file:///etc/passwd",
		"http:///nohost",
		"https://user:pass@example.com/hook",
		"://broken",
	}
	for _, raw := range bad {
		if _, err := ValidateHTTPURL(raw); err == nil {
			t.Errorf("ValidateHTTPURL(%q) accepted an invalid URL", raw)
		}
	}
}

func TestValidateHTTPURLAcceptsOrdinaryURLs(t *testing.T) {
	u, err := ValidateHTTPURL("https://hooks.slack.com/services/T/B/X")
	if err != nil {
		t.Fatalf("unexpected rejection: %v", err)
	}
	if u.Hostname() != "hooks.slack.com" {
		t.Fatalf("host = %q", u.Hostname())
	}
}

func TestIsBlockedIPRejectsInternalRanges(t *testing.T) {
	blocked := []string{
		"127.0.0.1",
		"127.10.0.1",
		"10.1.2.3",
		"172.16.0.1",
		"192.168.1.10",
		"169.254.169.254", // cloud instance metadata
		"100.64.0.1",      // CGNAT / container networks
		"0.0.0.0",
		"0.1.2.3",
		"198.18.0.1",
		"::1",
		"fd00::1",
		"fe80::1",
		"::ffff:169.254.169.254",
	}
	for _, raw := range blocked {
		if !IsBlockedIP(net.ParseIP(raw)) {
			t.Errorf("IsBlockedIP(%s) = false, want blocked", raw)
		}
	}

	allowed := []string{"8.8.8.8", "1.1.1.1", "2606:4700:4700::1111"}
	for _, raw := range allowed {
		if IsBlockedIP(net.ParseIP(raw)) {
			t.Errorf("IsBlockedIP(%s) = true, want allowed", raw)
		}
	}
}

func TestProviderPolicyAllowsLocalRuntimesButNotMetadata(t *testing.T) {
	// A self-hosted Ollama or vLLM is a supported deployment.
	for _, raw := range []string{"127.0.0.1", "10.0.0.5", "192.168.1.4", "::1"} {
		if IsBlockedProviderIP(net.ParseIP(raw)) {
			t.Errorf("provider policy blocked %s, which is a legitimate local runtime", raw)
		}
	}
	// No inference provider lives at a link-local address.
	for _, raw := range []string{"169.254.169.254", "fe80::1", "0.0.0.0"} {
		if !IsBlockedProviderIP(net.ParseIP(raw)) {
			t.Errorf("provider policy allowed %s", raw)
		}
	}
}

func TestValidatePublicURLRejectsInternalTargets(t *testing.T) {
	ctx := context.Background()
	for _, raw := range []string{
		"http://127.0.0.1:8080/hook",
		"http://localhost/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/hook",
		"http://[::1]/hook",
	} {
		if _, err := ValidatePublicURL(ctx, raw); err == nil {
			t.Errorf("ValidatePublicURL(%q) accepted an internal target", raw)
		}
	}
}

func TestValidatePublicURLRejectsCredentials(t *testing.T) {
	if _, err := ValidatePublicURL(context.Background(), "https://u:p@example.com/hook"); err == nil {
		t.Fatal("a URL with embedded credentials must be refused")
	}
}

func TestValidateProviderURLAllowsLoopbackButNotMetadata(t *testing.T) {
	ctx := context.Background()
	if _, err := ValidateProviderURL(ctx, "http://localhost:11434"); err != nil {
		t.Fatalf("a local runtime must be allowed: %v", err)
	}
	if _, err := ValidateProviderURL(ctx, "http://127.0.0.1:8000/v1"); err != nil {
		t.Fatalf("a loopback provider must be allowed: %v", err)
	}
	if _, err := ValidateProviderURL(ctx, "http://169.254.169.254/latest"); err == nil {
		t.Fatal("a provider pointing at cloud metadata must be refused")
	}
	if _, err := ValidateProviderURL(ctx, "http://[fe80::1]/v1"); err == nil {
		t.Fatal("a link-local provider must be refused")
	}
}

func TestDialContextRefusesBlockedAddress(t *testing.T) {
	dial := DialContext(&net.Dialer{Timeout: time.Second}, IsBlockedIP)
	if _, err := dial(context.Background(), "tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("the dialer must refuse a link-local address")
	}
	if _, err := dial(context.Background(), "tcp", "127.0.0.1:1"); err == nil {
		t.Fatal("the dialer must refuse loopback")
	}
}

func TestNewPublicClientRejectsUnsafeRedirect(t *testing.T) {
	client := NewPublicClient(time.Second)
	if client.CheckRedirect == nil {
		t.Fatal("a public client must install a redirect policy")
	}

	redirect := httptest.NewRequest(http.MethodGet, "http://example.com/next", nil)
	redirect.URL = mustURL(t, "file:///etc/passwd")
	if err := client.CheckRedirect(redirect, nil); err == nil {
		t.Fatal("a redirect to a non-http(s) URL must be refused")
	}

	tooMany := make([]*http.Request, maxRedirects)
	hop := httptest.NewRequest(http.MethodGet, "https://example.com/next", nil)
	if err := client.CheckRedirect(hop, tooMany); err == nil {
		t.Fatal("a redirect chain beyond the limit must be refused")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestBlockedRangeMessage(t *testing.T) {
	// A refused target should name the reason, not just "bad request".
	_, err := ValidatePublicURL(context.Background(), "http://169.254.169.254/")
	if err == nil || !strings.Contains(err.Error(), "non-public") {
		t.Fatalf("error = %v, want a non-public address explanation", err)
	}
}
