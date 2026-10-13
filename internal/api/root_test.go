package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/auth"
	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/version"
)

// getRoot issues GET / with the given Accept header against a minimal server.
func getRoot(t *testing.T, accept string) (int, http.Header, string) {
	t.Helper()
	cfg := config.Default()
	cfg.Admin.Enabled = false
	s, err := NewServer(Deps{
		Config:        cfg,
		Version:       version.Info{Version: "test-root"},
		Authenticator: auth.New(auth.Options{AdminKey: "root-test-admin-key-value"}),
		StartedAt:     time.Now().Add(-90 * time.Second),
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

// TestRootAnswersBrowsersWithAStatusPage guards the regression that a gateway
// reached from a browser (a temporary tunnel, a pasted URL) used to answer 404,
// which reads as a broken deployment rather than an API with no landing page.
func TestRootAnswersBrowsersWithAStatusPage(t *testing.T) {
	status, header, body := getRoot(t, "text/html,application/xhtml+xml")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	if ct := header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content type = %q, want text/html", ct)
	}
	for _, want := range []string{"Synapass", "test-root", "/v1/chat/completions", "API key"} {
		if !strings.Contains(body, want) {
			t.Errorf("landing page should mention %q", want)
		}
	}
}

func TestRootAnswersJSONClientsWithEndpoints(t *testing.T) {
	status, _, body := getRoot(t, "application/json")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("root must return valid JSON for JSON clients: %v (%s)", err, body)
	}
	if decoded["service"] != "synapass" {
		t.Errorf("service = %v", decoded["service"])
	}
	if decoded["status"] != "ok" {
		t.Errorf("status = %v", decoded["status"])
	}
	endpoints, ok := decoded["endpoints"].(map[string]any)
	if !ok || endpoints["chat"] == nil {
		t.Fatalf("endpoints must be listed: %s", body)
	}
}

func TestRootRequiresNoCredentialAndLeaksNothing(t *testing.T) {
	// The root is public like the health probes, so it must not disclose
	// anything they do not: no admin key, no tenant ids, no provider secrets.
	status, _, body := getRoot(t, "application/json")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 without a credential", status)
	}
	for _, forbidden := range []string{"root-test-admin-key-value", "Bearer "} {
		if strings.Contains(body, forbidden) {
			t.Errorf("root must not disclose %q", forbidden)
		}
	}
}
