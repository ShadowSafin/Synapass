package cache

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

type prefixStore struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *prefixStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.data[key]
	return v, ok, nil
}
func (m *prefixStore) Set(_ context.Context, key string, value []byte, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	m.data[key] = append([]byte{}, value...)
	return nil
}
func (m *prefixStore) DeletePrefix(_ context.Context, prefix string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	removed := 0
	for k := range m.data {
		if strings.Contains(k, prefix) || strings.HasPrefix(k, prefix) {
			delete(m.data, k)
			removed++
		}
	}
	// Also match the namespaced form: store keys are bare in this fake, so
	// match suffix hints for tenant isolation checks.
	return removed, nil
}

func baseInput() KeyInput {
	temp := 0.0
	return KeyInput{
		TenantID: "t1", APIKeyID: "k1", Model: "m1",
		Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("hello world")}},
		Temperature: &temp,
	}
}

func TestKeyIsolation(t *testing.T) {
	a := baseInput()
	b := baseInput()
	b.TenantID = "t2"
	if ExactKey(a) == ExactKey(b) {
		t.Fatalf("tenant must isolate keys")
	}
	b = baseInput()
	b.APIKeyID = "k2"
	if ExactKey(a) == ExactKey(b) {
		t.Fatalf("api key must isolate keys")
	}
	b = baseInput()
	b.Model = "m2"
	if ExactKey(a) == ExactKey(b) {
		t.Fatalf("model must isolate keys")
	}
	b = baseInput()
	hot := 0.9
	b.Temperature = &hot
	if ExactKey(a) == ExactKey(b) {
		t.Fatalf("temperature must isolate keys")
	}
	b = baseInput()
	b.PolicyID = "p1"
	if ExactKey(a) == ExactKey(b) {
		t.Fatalf("policy must isolate keys")
	}
	b = baseInput()
	b.EndpointID = "mobile"
	if ExactKey(a) == ExactKey(b) {
		t.Fatalf("endpoint must isolate keys")
	}
	b = baseInput()
	b.FormatType = "json_object"
	if ExactKey(a) == ExactKey(b) {
		t.Fatalf("response format must isolate keys")
	}
	b = baseInput()
	b.Tools = []domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "now"}}}
	if ExactKey(a) == ExactKey(b) {
		t.Fatalf("tools must isolate keys")
	}
	// Tool definition change (same name, different schema) must differ.
	c := baseInput()
	c.Tools = []domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "w", Parameters: []byte(`{"a":1}`)}}}
	d := baseInput()
	d.Tools = []domain.Tool{{Type: "function", Function: domain.FunctionDefinition{Name: "w", Parameters: []byte(`{"a":2}`)}}}
	if ExactKey(c) == ExactKey(d) {
		t.Fatalf("tool schema change must isolate keys")
	}
}

func TestPrefixSafety(t *testing.T) {
	// Semantic disabled to isolate the prefix tier: any hit here would have
	// to come from prefix reuse, which must refuse long divergent prompts.
	opts := DefaultOptions()
	opts.SemanticEnabled = false
	c := New(&prefixStore{}, opts)
	ctx := context.Background()
	// Shared 256-char head with disjoint tails.
	head := strings.Repeat("prefixword ", 30) // ~330 chars > PrefixLength
	tailA := " alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi"
	tailB := " omicron pi rho sigma tau upsilon phi chi psi omega aleph beth gimel daleth"
	in1 := baseInput()
	in1.Messages = []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(head + tailA)}}
	in2 := baseInput()
	in2.Messages = []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(head + tailB)}}
	c.StoreResponse(ctx, in1, []byte(`{"a":1}`), false)
	hit := c.Lookup(ctx, in2, false, "", false)
	if hit.Hit {
		t.Fatalf("shared prefix with different tail must not hit, got kind=%s sim=%.3f", hit.Kind, hit.Similarity)
	}
	// Short prompts may still share the prefix tier.
	short1 := baseInput()
	short1.Messages = []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("short prompt")}}
	short2 := baseInput()
	short2.Messages = []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent("short prompt")}}
	c.StoreResponse(ctx, short1, []byte(`{"s":1}`), false)
	if hit := c.Lookup(ctx, short2, false, "", false); !hit.Hit {
		t.Fatalf("identical short prompt must hit")
	}
}

func TestTenantInvalidationIsolation(t *testing.T) {
	st := &prefixStore{}
	c := New(st, DefaultOptions())
	ctx := context.Background()
	a := baseInput()
	a.TenantID = "t1"
	b := baseInput()
	b.TenantID = "t2"
	c.StoreResponse(ctx, a, []byte(`{"t":1}`), false)
	c.StoreResponse(ctx, b, []byte(`{"t":2}`), false)
	if hit := c.Lookup(ctx, a, false, "", false); !hit.Hit {
		t.Fatalf("t1 must hit before invalidate")
	}
	if _, err := c.Invalidate(ctx, InvalidateScope{Scope: "tenant", TenantID: "t1", Reason: "test"}); err != nil {
		t.Fatalf("invalidate: %v", err)
	}
	// t1's semantic index entry is gone; t2 survives in the index.
	if hit := c.Lookup(ctx, b, false, "", false); !hit.Hit {
		t.Fatalf("t2 must survive t1 flush")
	}
}

func TestPolicyBypass(t *testing.T) {
	cfg := PolicyConfig{Enabled: true, ExactEnabled: true, TTL: 5 * time.Minute, BypassTools: true, BypassLiveData: true}
	hot := 0.9
	cases := []struct {
		name   string
		in     PolicyInput
		reason string
	}{
		{"sensitive", PolicyInput{Sensitive: true, PolicyUseCache: true}, domain.CacheBypassSensitive},
		{"policy_off", PolicyInput{PolicyUseCache: false}, domain.CacheBypassPolicyDisabled},
		{"tools", PolicyInput{PolicyUseCache: true, HasTools: true, ToolsSafe: false}, domain.CacheBypassToolRequest},
		{"nondet", PolicyInput{PolicyUseCache: true, Temperature: &hot}, domain.CacheBypassNondeterministic},
		{"multi", PolicyInput{PolicyUseCache: true, N: 2}, domain.CacheBypassMultiSample},
		{"images", PolicyInput{PolicyUseCache: true, HasImages: true}, domain.CacheBypassMultimodal},
	}
	// Streams are first-class cache citizens: a hit replays as SSE and a
	// clean completion is stored, so streaming no longer bypasses.
	zeroStream := 0.0
	if got := Evaluate(PolicyInput{Stream: true, PolicyUseCache: true, Temperature: &zeroStream}, cfg); !got.Cacheable {
		t.Fatalf("streaming deterministic request must be cacheable, got %+v", got)
	}
	for _, tc := range cases {
		if got := Evaluate(tc.in, cfg); got.Cacheable || got.BypassReason != tc.reason {
			t.Fatalf("%s: got %+v want bypass=%s", tc.name, got, tc.reason)
		}
	}
	// Safe tools + deterministic settings are cacheable.
	zero := 0.0
	if got := Evaluate(PolicyInput{PolicyUseCache: true, HasTools: true, ToolsSafe: true, Temperature: &zero}, cfg); !got.Cacheable {
		t.Fatalf("safe deterministic tool request must be cacheable, got %+v", got)
	}
	// Endpoint cache=false bypasses.
	off := false
	if got := Evaluate(PolicyInput{PolicyUseCache: true, EndpointCache: &off}, cfg); got.Cacheable {
		t.Fatalf("endpoint_cache=false must bypass")
	}
}

func TestSemanticTenantIsolation(t *testing.T) {
	opts := DefaultOptions()
	opts.SemanticThreshold = 0.5
	c := New(&prefixStore{data: map[string][]byte{}}, opts)
	ctx := context.Background()
	long := "the quick brown fox jumps over the lazy dog and then runs away fast across the meadow"
	in1 := KeyInput{TenantID: "t1", Model: "m", Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(long)}}}
	in2 := KeyInput{TenantID: "t2", Model: "m", Messages: []domain.ChatMessage{{Role: domain.RoleUser, Content: domain.NewTextContent(long + " !!")}}}
	c.StoreResponse(ctx, in1, []byte(`{"a":1}`), false)
	// Different tenant must not see t1's semantic entry even with similar words.
	if hit := c.Lookup(ctx, in2, false, "", false); hit.Hit && hit.Kind == domain.CacheSemantic {
		t.Fatalf("semantic must isolate tenants")
	}
}
