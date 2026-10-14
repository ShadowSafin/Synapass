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
	"github.com/shadowsafin/synapass/internal/benchmark"
	"github.com/shadowsafin/synapass/internal/config"
	"github.com/shadowsafin/synapass/internal/policy"
	"github.com/shadowsafin/synapass/internal/providers"
	"github.com/shadowsafin/synapass/internal/routing"
	"github.com/shadowsafin/synapass/internal/version"
)

// newBenchmarkTestServer builds a server with the admin surface enabled and
// the benchmark engine in the requested state. Repositories are deliberately
// absent: these tests cover route gating, not the store.
func newBenchmarkTestServer(t *testing.T, enabled bool) *httptest.Server {
	t.Helper()

	cfg := config.Default()
	cfg.Admin.Enabled = true
	cfg.Benchmark.Enabled = enabled

	health := routing.NewHealthTracker(routing.HealthConfig{})
	engine := benchmark.New(benchmark.Options{Config: cfg.Benchmark})

	server, err := NewServer(Deps{
		Config:        cfg,
		Version:       version.Info{Version: "test"},
		Authenticator: auth.New(auth.Options{Store: &stubKeyStore{}, AdminKey: testAdminKey}),
		Engine:        routing.NewEngine(routing.NewStaticCatalogue(nil, nil), nil, health),
		Executor:      routing.NewExecutor(providers.NewRegistry(), health),
		Health:        health,
		Policies:      policy.NewResolver(&stubPolicyRepo{}, time.Minute),
		Adapters:      providers.NewRegistry(),
		Benchmark:     engine,
	})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	srv := httptest.NewServer(server.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func benchmarkPost(t *testing.T, srv *httptest.Server, path, token, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

func benchmarkGet(t *testing.T, srv *httptest.Server, path, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// When the engine is disabled the dashboard must learn that from /status,
// while every other benchmark route stays unregistered.
func TestBenchmarkRoutesAreAbsentWhenDisabled(t *testing.T) {
	srv := newBenchmarkTestServer(t, false)

	status, body := benchmarkGet(t, srv, "/admin/v1/benchmark/status", testAdminKey)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var decoded struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if decoded.Enabled {
		t.Error("a disabled engine must report enabled=false")
	}

	for _, path := range []string{
		"/admin/v1/benchmark/jobs",
		"/admin/v1/benchmark/overview",
		"/admin/v1/benchmark/history",
		"/admin/v1/benchmark/report",
		"/admin/v1/benchmark/health",
		"/admin/v1/benchmark/providers",
		"/admin/v1/benchmark/models",
	} {
		if status, body := benchmarkGet(t, srv, path, testAdminKey); status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 when disabled: %s", path, status, body)
		}
	}
}

// When enabled the status reports true and the routes exist. The store is
// absent in this harness, so data routes fail with a store error rather than
// 404 — which is exactly the signal that the route is registered.
func TestBenchmarkRoutesExistWhenEnabled(t *testing.T) {
	srv := newBenchmarkTestServer(t, true)

	status, body := benchmarkGet(t, srv, "/admin/v1/benchmark/status", testAdminKey)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", status, body)
	}
	var decoded struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if !decoded.Enabled {
		t.Error("an enabled engine must report enabled=true")
	}

	for _, path := range []string{
		"/admin/v1/benchmark/jobs",
		"/admin/v1/benchmark/overview",
		"/admin/v1/benchmark/history",
		"/admin/v1/benchmark/health",
	} {
		if status, body := benchmarkGet(t, srv, path, testAdminKey); status == http.StatusNotFound {
			t.Errorf("GET %s = 404, want the route registered when enabled: %s", path, body)
		}
	}
}

// The status endpoint still requires authentication: it lives under /admin/v1.
func TestBenchmarkStatusRejectsAnonymousAccess(t *testing.T) {
	srv := newBenchmarkTestServer(t, false)

	if status, _ := benchmarkGet(t, srv, "/admin/v1/benchmark/status", ""); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for anonymous access", status)
	}
}

// The dashboard toggle enables the engine at runtime without a restart:
// POST /benchmark/enabled flips the switch, data routes come alive, and
// flipping it back disables them again.
func TestBenchmarkToggleEnablesAtRuntime(t *testing.T) {
	srv := newBenchmarkTestServer(t, false)

	if status, _ := benchmarkGet(t, srv, "/admin/v1/benchmark/jobs", testAdminKey); status != http.StatusNotFound {
		t.Fatalf("jobs = %d before enabling, want 404", status)
	}

	status, body := benchmarkPost(t, srv, "/admin/v1/benchmark/enabled", testAdminKey, `{"enabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("enable = %d, want 200: %s", status, body)
	}
	var decoded struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !decoded.Enabled {
		t.Error("toggle response must report enabled=true")
	}

	if status, _ := benchmarkGet(t, srv, "/admin/v1/benchmark/status", testAdminKey); status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	// Routes are registered; without a store they fail past the 404 gate.
	if status, _ := benchmarkGet(t, srv, "/admin/v1/benchmark/jobs", testAdminKey); status == http.StatusNotFound {
		t.Error("jobs = 404 after enabling, want the route alive")
	}

	status, body = benchmarkPost(t, srv, "/admin/v1/benchmark/enabled", testAdminKey, `{"enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("disable = %d, want 200: %s", status, body)
	}
	if status, _ := benchmarkGet(t, srv, "/admin/v1/benchmark/jobs", testAdminKey); status != http.StatusNotFound {
		t.Fatalf("jobs = %d after disabling, want 404", status)
	}
}

// The toggle itself requires full admin scope: it starts background workers.
func TestBenchmarkToggleRejectsAnonymousAccess(t *testing.T) {
	srv := newBenchmarkTestServer(t, false)

	if status, _ := benchmarkPost(t, srv, "/admin/v1/benchmark/enabled", "", `{"enabled":true}`); status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 for anonymous access", status)
	}
}
