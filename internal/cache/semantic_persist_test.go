package cache

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// persistStore is a TTL-enforcing fake with SCAN support. It mirrors the
// production redisCacheStore mapping (logical keys under "response:") so
// invalidation prefix strings behave exactly like production.
type persistStore struct {
	mu       sync.Mutex
	data     map[string][]byte
	deadline map[string]time.Time
	ttlOf    map[string]time.Duration
}

func newPersistStore() *persistStore {
	return &persistStore{
		data:     map[string][]byte{},
		deadline: map[string]time.Time{},
		ttlOf:    map[string]time.Duration{},
	}
}

func (m *persistStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	full := "response:" + key
	v, ok := m.data[full]
	if !ok {
		return nil, false, nil
	}
	if dl, has := m.deadline[full]; has && time.Now().After(dl) {
		delete(m.data, full)
		delete(m.deadline, full)
		return nil, false, nil
	}
	return append([]byte{}, v...), true, nil
}

func (m *persistStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	full := "response:" + key
	m.data[full] = append([]byte{}, value...)
	if ttl > 0 {
		m.deadline[full] = time.Now().Add(ttl)
	} else {
		delete(m.deadline, full)
	}
	m.ttlOf[full] = ttl
	return nil
}

func (m *persistStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	mapped := mapPrefix(prefix)
	removed := 0
	for k := range m.data {
		if strings.HasPrefix(k, mapped) {
			delete(m.data, k)
			delete(m.deadline, k)
			removed++
		}
	}
	return removed, nil
}

func (m *persistStore) Scan(_ context.Context, pattern string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	full := pattern
	if !strings.HasPrefix(full, "response:") {
		full = "response:" + full
	}
	prefix := strings.TrimSuffix(full, "*")
	var out []string
	for k := range m.data {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		if dl, has := m.deadline[k]; has && time.Now().After(dl) {
			continue
		}
		out = append(out, strings.TrimPrefix(k, "response:"))
	}
	return out, nil
}

// keysWith lists physical keys under the logical prefix (for example
// "sidx:" or "tenant:t1:semantic:").
func (m *persistStore) keysWith(prefix string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.data {
		if strings.HasPrefix(k, "response:"+prefix) {
			out = append(out, k)
		}
	}
	return out
}

func (m *persistStore) ttlFor(physicalKey string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.ttlOf[physicalKey]
	return v, ok
}

func persistInput(tenant, prompt string) KeyInput {
	return KeyInput{
		TenantID: tenant, Model: "m",
		Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(prompt)}},
	}
}

const persistLong = "the quick brown fox jumps over the lazy dog and then runs away fast across the green meadow"

// TestSemanticPersistAndRebuild proves a restart does not cold-start semantic
// reuse: the sidecar written at store time lets a fresh Cache warm its index
// and serve a near-duplicate from the semantic tier.
func TestSemanticPersistAndRebuild(t *testing.T) {
	st := newPersistStore()
	opts := DefaultOptions()
	opts.SemanticThreshold = 0.4
	c1 := New(st, opts)
	ctx := context.Background()
	in := persistInput("t1", persistLong)
	mustStore(t, c1, in, `{"a":1}`)

	// The sidecar exists next to the body and shares its TTL.
	bodies := st.keysWith("tenant:t1:semantic:")
	if len(bodies) != 1 {
		t.Fatalf("expected one semantic body, got %v", bodies)
	}
	sidecars := st.keysWith("sidx:")
	if len(sidecars) != 1 {
		t.Fatalf("expected one semantic sidecar, got %v", sidecars)
	}
	bodyTTL, ok := st.ttlFor(bodies[0])
	if !ok {
		t.Fatalf("body TTL was not recorded")
	}
	sideTTL, ok := st.ttlFor(sidecars[0])
	if !ok {
		t.Fatalf("sidecar TTL was not recorded")
	}
	if bodyTTL != sideTTL {
		t.Fatalf("sidecar TTL %v must match body TTL %v", sideTTL, bodyTTL)
	}

	// Fresh process, same Redis: warm and serve.
	c2 := New(st, opts)
	if n, err := c2.Warmup(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	} else if n != 1 {
		t.Fatalf("warmup must repopulate 1 entry, got %d", n)
	}
	near := persistInput("t1", persistLong+" !!")
	hit := c2.Lookup(ctx, near, false, "", false)
	if !hit.Hit || hit.Kind != domain.CacheSemantic {
		t.Fatalf("rebuilt index must serve a semantic hit, got %+v", hit)
	}
	if string(hit.Body) != `{"a":1}` {
		t.Fatalf("rebuilt hit body mismatch: %s", hit.Body)
	}
}

// TestSemanticSidecarPerRequestTTL proves a per-request TTL bound applies to
// the sidecar too, so a short-lived entry does not outlive its body in the
// warmup set.
func TestSemanticSidecarPerRequestTTL(t *testing.T) {
	st := newPersistStore()
	c := New(st, DefaultOptions())
	ctx := context.Background()
	in := persistInput("t1", "per-request ttl probe with enough words to embed")
	meta := domain.CacheHitMeta{ExpiresAt: time.Now().Add(30 * time.Second)}
	if got := c.StoreResponseWithMeta(ctx, in, []byte(`{"x":1}`), false, meta); got == "" {
		t.Fatalf("short-TTL envelope must be stored")
	}
	var bodyTTL, sideTTL time.Duration
	var bodyOK, sideOK bool
	for _, k := range st.keysWith("tenant:t1:semantic:") {
		bodyTTL, bodyOK = st.ttlFor(k)
	}
	for _, k := range st.keysWith("sidx:") {
		sideTTL, sideOK = st.ttlFor(k)
	}
	if !bodyOK || !sideOK {
		t.Fatalf("both body and sidecar must be written")
	}
	if bodyTTL > 31*time.Second || sideTTL > 31*time.Second {
		t.Fatalf("per-request bound ignored: body=%v sidecar=%v", bodyTTL, sideTTL)
	}
	if bodyTTL != sideTTL {
		t.Fatalf("sidecar TTL %v must match body TTL %v", sideTTL, bodyTTL)
	}
}

// TestSemanticWarmupSkipsExpired proves expiry is honored on rebuild: an
// entry whose TTL lapsed never re-enters the index, and a sidecar whose
// body is gone is skipped rather than indexed as a dangling pointer.
func TestSemanticWarmupSkipsExpired(t *testing.T) {
	ctx := context.Background()

	// Lapsed TTL: both body and sidecar are dead by warmup time.
	st := newPersistStore()
	c := New(st, DefaultOptions())
	in := persistInput("t1", "lapsed ttl probe with enough words to embed")
	short := domain.CacheHitMeta{ExpiresAt: time.Now().Add(80 * time.Millisecond)}
	if got := c.StoreResponseWithMeta(ctx, in, []byte(`{"x":1}`), false, short); got == "" {
		t.Fatalf("short-TTL envelope must be stored")
	}
	time.Sleep(150 * time.Millisecond)
	c2 := New(st, DefaultOptions())
	if n, err := c2.Warmup(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	} else if n != 0 {
		t.Fatalf("expired entries must not warm, got %d", n)
	}
	mustMiss(t, c2, in)

	// Dangling sidecar: every body evicted without its sidecar.
	st2 := newPersistStore()
	c3 := New(st2, DefaultOptions())
	in2 := persistInput("t1", "dangling sidecar probe with enough words to embed")
	mustStore(t, c3, in2, `{"y":1}`)
	// Remove every body tier (exact + prefix + semantic) while leaving the
	// sidecar behind.
	for _, k := range st2.keysWith("tenant:t1:") {
		// keysWith returns physical keys; the fake's DeletePrefix matches
		// them literally, deleting exactly that body.
		if _, err := st2.DeletePrefix(ctx, k); err != nil {
			t.Fatalf("delete body: %v", err)
		}
	}
	if got := st2.keysWith("sidx:"); len(got) != 1 {
		t.Fatalf("sidecar must survive the body delete, got %v", got)
	}
	c4 := New(st2, DefaultOptions())
	if n, err := c4.Warmup(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	} else if n != 0 {
		t.Fatalf("dangling sidecar must be skipped, got %d", n)
	}
	mustMiss(t, c4, in2)
}

// TestSemanticWarmupTenantIsolation proves a rebuilt index still isolates
// tenants: another tenant's near-duplicate must miss while the owning
// tenant hits.
func TestSemanticWarmupTenantIsolation(t *testing.T) {
	st := newPersistStore()
	opts := DefaultOptions()
	opts.SemanticThreshold = 0.4
	c1 := New(st, opts)
	ctx := context.Background()
	mustStore(t, c1, persistInput("t1", persistLong), `{"a":1}`)

	c2 := New(st, opts)
	if n, err := c2.Warmup(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	} else if n != 1 {
		t.Fatalf("warmup must repopulate 1 entry, got %d", n)
	}
	if hit := c2.Lookup(ctx, persistInput("t2", persistLong+" !!"), false, "", false); hit.Hit {
		t.Fatalf("cross-tenant reuse after warmup: kind=%s sim=%.3f", hit.Kind, hit.Similarity)
	}
	hit := c2.Lookup(ctx, persistInput("t1", persistLong+" !!"), false, "", false)
	if !hit.Hit || hit.Kind != domain.CacheSemantic {
		t.Fatalf("owning tenant must hit after warmup, got %+v", hit)
	}
}

// TestSemanticWarmupBounded proves the rebuild respects MaxSemanticEntries
// with oldest-first eviction: the newest entries survive, the oldest do not
// re-enter the search index.
func TestSemanticWarmupBounded(t *testing.T) {
	st := newPersistStore()
	opts := DefaultOptions()
	opts.MaxSemanticEntries = 8
	c1 := New(st, opts)
	ctx := context.Background()
	inputs := make([]KeyInput, 0, 20)
	for i := 0; i < 20; i++ {
		in := persistInput("t1", fmt.Sprintf("warmup bound probe %d", i))
		// Distinct single-token prompts keep setup fast; the bound
		// assertion inspects index membership directly, not similarity.
		inputs = append(inputs, in)
		mustStore(t, c1, in, fmt.Sprintf(`{"i":%d}`, i))
		// Distinct storedAt ordering for the oldest-first bound.
		time.Sleep(time.Millisecond)
	}

	c2 := New(st, opts)
	n, err := c2.Warmup(ctx)
	if err != nil {
		t.Fatalf("warmup: %v", err)
	}
	if n != 8 {
		t.Fatalf("warmup must be bounded at 8, got %d", n)
	}
	c2.mu.Lock()
	indexLen := len(c2.memIndex)
	c2.mu.Unlock()
	if indexLen != 8 {
		t.Fatalf("rebuilt index must hold 8 entries, got %d", indexLen)
	}
	nsOf := func(in KeyInput) string {
		return namespaced(in.TenantID, "semantic:"+embeddingHash(embed(in)))
	}
	c2.mu.Lock()
	_, newestKept := c2.memIndex[nsOf(inputs[19])]
	_, oldestKept := c2.memIndex[nsOf(inputs[0])]
	c2.mu.Unlock()
	if !newestKept {
		t.Fatalf("newest entry must survive a bounded warmup")
	}
	if oldestKept {
		t.Fatalf("oldest entry must be evicted by a bounded warmup")
	}
}

// TestWarmupWithoutScanner proves stores that cannot enumerate (plain maps)
// degrade to a no-op rather than an error, so warmup never fails startup.
func TestWarmupWithoutScanner(t *testing.T) {
	c := New(&memStore{}, DefaultOptions())
	if n, err := c.Warmup(context.Background()); err != nil || n != 0 {
		t.Fatalf("non-scanning store must warm to (0, nil), got (%d, %v)", n, err)
	}
	disabled := DefaultOptions()
	disabled.SemanticEnabled = false
	cOff := New(newPersistStore(), disabled)
	if n, err := cOff.Warmup(context.Background()); err != nil || n != 0 {
		t.Fatalf("disabled tier must warm to (0, nil), got (%d, %v)", n, err)
	}
}

// TestSemanticInvalidationClearsSidecars proves tenant and key flushes evict
// the durable sidecars too, so a flushed entry cannot crawl back on the
// next restart's warmup.
func TestSemanticInvalidationClearsSidecars(t *testing.T) {
	ctx := context.Background()

	// Tenant scope.
	st := newPersistStore()
	opts := DefaultOptions()
	opts.SemanticThreshold = 0.4
	c := New(st, opts)
	mustStore(t, c, persistInput("t1", persistLong), `{"a":1}`)
	mustStore(t, c, persistInput("t2", persistLong), `{"b":1}`)
	if _, err := c.Invalidate(ctx, InvalidateScope{Scope: "tenant", TenantID: "t1", Reason: "test"}); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	for _, k := range st.keysWith("sidx:") {
		raw, ok, _ := st.Get(ctx, strings.TrimPrefix(k, "response:"))
		if !ok {
			continue
		}
		if strings.Contains(string(raw), `"tenant":"t1"`) {
			t.Fatalf("t1 sidecar survived tenant flush: %s", k)
		}
	}
	c2 := New(st, opts)
	if n, err := c2.Warmup(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	} else if n != 1 {
		t.Fatalf("only t2 must rewarm, got %d", n)
	}

	// Key scope removes the sidecar with the body.
	st3 := newPersistStore()
	c3 := New(st3, opts)
	in := persistInput("t1", persistLong)
	mustStore(t, c3, in, `{"k":1}`)
	if len(st3.keysWith("sidx:")) != 1 {
		t.Fatalf("expected one sidecar before key flush")
	}
	if _, err := c3.Invalidate(ctx, InvalidateScope{Scope: "key", TenantID: "t1", Key: ExactKey(in), Reason: "test"}); err != nil {
		t.Fatalf("key invalidate: %v", err)
	}
	if got := st3.keysWith("sidx:"); len(got) != 0 {
		t.Fatalf("key flush must remove the sidecar, left %v", got)
	}
	c4 := New(st3, opts)
	if n, err := c4.Warmup(ctx); err != nil {
		t.Fatalf("warmup: %v", err)
	} else if n != 0 {
		t.Fatalf("flushed entry must not rewarm, got %d", n)
	}
}
