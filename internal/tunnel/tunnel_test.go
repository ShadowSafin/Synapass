package tunnel

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

func TestParseURL(t *testing.T) {
	cases := []struct {
		line string
		want string
	}{
		{
			"2026-01-01T00:00:00Z INF Your quick Tunnel has been created! Visit it at (URLs): https://steel-pandas-happen.trycloudflare.com",
			"https://steel-pandas-happen.trycloudflare.com",
		},
		{"ERR failed to connect", ""},
		{"listening on http://127.0.0.1:8080 with no url", ""},
		{
			"INF Registered tunnel connection id=1 location=fra https://abc-123.cfargotunnel.com",
			"https://abc-123.cfargotunnel.com",
		},
	}
	for _, c := range cases {
		if got := ParseURL(c.line); got != c.want {
			t.Errorf("ParseURL(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}

func TestResolveTarget(t *testing.T) {
	m := NewManager(Config{
		DefaultTarget: "gateway", DashboardTarget: "http://127.0.0.1:3000",
		AllowCustomTargets: true,
	}, "http://127.0.0.1:8080", nil, nil)

	name, addr, err := m.ResolveTarget("")
	if err != nil || name != "gateway" || addr != "http://127.0.0.1:8080" {
		t.Errorf("empty target = %q %q %v", name, addr, err)
	}
	if _, addr, err := m.ResolveTarget("dashboard"); err != nil || addr != "http://127.0.0.1:3000" {
		t.Errorf("dashboard = %q %v", addr, err)
	}
	if _, addr, err := m.ResolveTarget("127.0.0.1:9999"); err != nil || addr != "http://127.0.0.1:9999" {
		t.Errorf("loopback = %q %v", addr, err)
	}
	for _, bad := range []string{
		"example.com:8080", "http://127.0.0.1:8080", "127.0.0.1",
		"127.0.0.1:0", "127.0.0.1:99999", "10.0.0.1:8080", "evil.com",
		"127.0.0.1:8080/path", "[::1]",
	} {
		if _, _, err := m.ResolveTarget(bad); err == nil {
			t.Errorf("target %q must be rejected", bad)
		}
	}
	if _, addr, err := m.ResolveTarget("[::1]:8080"); err != nil || addr != "http://::1:8080" {
		t.Errorf("bracketed ipv6 = %q %v", addr, err)
	}

	locked := NewManager(Config{AllowCustomTargets: false}, "http://127.0.0.1:8080", nil, nil)
	if _, _, err := locked.ResolveTarget("127.0.0.1:9999"); err == nil {
		t.Error("custom targets must be rejected when disabled")
	}
	if _, _, err := locked.ResolveTarget("gateway"); err != nil {
		t.Errorf("named targets must still work when custom is disabled: %v", err)
	}
}

// fakeStore is an in-memory Store.
type fakeStore struct {
	mu   sync.Mutex
	rows map[string]*domain.TunnelSession
}

func (f *fakeStore) Create(_ context.Context, s *domain.TunnelSession) (*domain.TunnelSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows == nil {
		f.rows = map[string]*domain.TunnelSession{}
	}
	if s.ID == "" {
		s.ID = domain.NewID()
	}
	now := domain.Now()
	s.CreatedAt = now
	s.UpdatedAt = now
	cp := *s
	f.rows[s.ID] = &cp
	return &cp, nil
}

func (f *fakeStore) Update(_ context.Context, s *domain.TunnelSession) (*domain.TunnelSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *s
	cp.UpdatedAt = domain.Now()
	f.rows[s.ID] = &cp
	return &cp, nil
}

func (f *fakeStore) GetByID(_ context.Context, id string) (*domain.TunnelSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.rows[id]; ok {
		cp := *s
		return &cp, nil
	}
	return nil, nil
}

func (f *fakeStore) Active(context.Context) (*domain.TunnelSession, error) { return nil, nil }

func (f *fakeStore) Recent(context.Context, int) ([]domain.TunnelSession, error) {
	return nil, nil
}

func (f *fakeStore) MarkStaleStopped(context.Context, string) (int, error) { return 0, nil }

// fakeBinary writes a script that mimics cloudflared: it prints a quick
// tunnel URL to stderr, then sleeps until killed.
func fakeBinary(t *testing.T, url string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-cloudflared.sh")
	content := "#!/bin/sh\necho \"2026-01-01T00:00:00Z INF Your quick Tunnel has been created! Visit it at (URLs): " + url + "\" >&2\nexec sleep 300\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

func TestCreateStopLifecycle(t *testing.T) {
	store := &fakeStore{}
	m := NewManager(Config{
		Binary:         fakeBinary(t, "https://test-tunnel-123.trycloudflare.com"),
		StartupTimeout: 10 * time.Second,
	}, "http://127.0.0.1:8080", store, nil)

	ctx := context.Background()
	s, err := m.Create(ctx, "gateway", "", "test")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Status != domain.TunnelRunning {
		t.Fatalf("status = %q, want running", s.Status)
	}
	if s.PublicURL != "https://test-tunnel-123.trycloudflare.com" {
		t.Fatalf("url = %q", s.PublicURL)
	}
	if got := m.Status(); got == nil || got.PublicURL != s.PublicURL {
		t.Fatalf("Status() did not reflect the active session: %+v", got)
	}

	// A second create while one is active must fail, not replace.
	if _, err := m.Create(ctx, "gateway", "", "test"); err == nil {
		t.Fatal("second create while active must fail")
	} else if !strings.Contains(err.Error(), "already") {
		t.Fatalf("conflict error must say so: %v", err)
	}

	stopped, err := m.Stop(ctx, "", "test done", "test")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.Status != domain.TunnelStopped {
		t.Fatalf("stopped status = %q", stopped.Status)
	}
	if m.Status() != nil {
		t.Fatal("Status() must be nil after stop")
	}

	// Stopping again is a safe no-op surface (row returned when known).
	if _, err := m.Stop(ctx, stopped.ID, "", "test"); err != nil {
		t.Fatalf("repeat stop must not fail: %v", err)
	}
}

func TestCreateFailsWithoutBinary(t *testing.T) {
	m := NewManager(Config{Binary: "definitely-not-a-real-binary-xyz"}, "http://127.0.0.1:8080", &fakeStore{}, nil)
	if _, err := m.Create(context.Background(), "gateway", "", "test"); err == nil {
		t.Fatal("Create must fail when cloudflared is missing")
	} else if !strings.Contains(err.Error(), "cloudflared is not installed") {
		t.Fatalf("missing-binary error must guide installation: %v", err)
	}
}

func TestCreateRejectsBadTarget(t *testing.T) {
	m := NewManager(Config{}, "http://127.0.0.1:8080", &fakeStore{}, nil)
	if _, err := m.Create(context.Background(), "example.com:8080", "", "test"); err == nil {
		t.Fatal("non-loopback target must be rejected before any process spawns")
	}
}
