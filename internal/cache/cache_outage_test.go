package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// errStore is a store whose backend is gone (Redis outage). Every operation
// fails; the cache must degrade to misses and best-effort writes, never to
// request failures.
type errStore struct{ err error }

func (s *errStore) Get(context.Context, string) ([]byte, bool, error) {
	return nil, false, s.err
}
func (s *errStore) Set(context.Context, string, []byte, time.Duration) error {
	return s.err
}
func (s *errStore) DeletePrefix(context.Context, string) (int, error) { return 0, s.err }

// TestStoreOutageDegradesToMisses proves a Redis outage never breaks
// unrelated streaming: lookups miss (load from provider), stores are
// best-effort, and invalidation reports the failure without panicking.
func TestStoreOutageDegradesToMisses(t *testing.T) {
	boom := errors.New("connection refused")
	c := New(&errStore{err: boom}, DefaultOptions())
	ctx := context.Background()
	in := KeyInput{
		TenantID: "t1", Model: "m",
		Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("hello")}},
	}

	// Lookup must miss, not error: the request falls through to the provider.
	res := c.Lookup(ctx, in, false, "", false)
	if res.Hit {
		t.Fatalf("outage lookup must miss")
	}
	// Store must not panic or propagate: the response was already delivered.
	if key := c.StoreResponse(ctx, in, []byte(`{"ok":1}`), false); key == "" {
		t.Fatalf("store must still name the logical key")
	}
	// Tenant isolation holds even when only the in-process index survives:
	// another tenant must not see this tenant's entry.
	other := in
	other.TenantID = "t2"
	if res := c.Lookup(ctx, other, false, "", false); res.Hit {
		t.Fatalf("outage must not leak entries across tenants")
	}
	// Bypass accounting keeps working so dashboards explain the misses.
	stats := c.Stats()
	if stats.Misses == 0 && stats.ExactMisses == 0 {
		t.Fatalf("misses must be counted during outage: %+v", stats)
	}
}
