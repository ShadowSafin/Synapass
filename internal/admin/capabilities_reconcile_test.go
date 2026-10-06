package admin

import (
	"context"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
)

// memProviderStore is an in-memory ProviderStore.
type memProviderStore struct {
	rows map[string]domain.Provider
}

func newMemProviderStore(providers ...domain.Provider) *memProviderStore {
	m := &memProviderStore{rows: map[string]domain.Provider{}}
	for _, p := range providers {
		m.rows[p.ID] = p
	}
	return m
}

func (m *memProviderStore) GetByID(_ context.Context, id string) (*domain.Provider, error) {
	p, ok := m.rows[id]
	if !ok {
		return nil, nil
	}
	return &p, nil
}

func (m *memProviderStore) Upsert(_ context.Context, p *domain.Provider) (*domain.Provider, error) {
	m.rows[p.ID] = *p
	return p, nil
}

// An empty provider row learns the union of what its models declare: this is
// the step that makes a sync that found tools actually route tools.
func TestReconcileFillsEmptyProvider(t *testing.T) {
	providers := newMemProviderStore(domain.Provider{
		ID: "p1", Name: "acme", Kind: domain.ProviderOpenAICompatible, BaseURL: "https://acme.test/v1",
	})
	models := &memModelStore{rows: []domain.Model{
		{ID: "a", ProviderID: "p1", Name: "m1", Capabilities: []domain.Capability{domain.CapChat, domain.CapTools}},
		{ID: "b", ProviderID: "p1", Name: "m2", Capabilities: []domain.Capability{domain.CapChat, domain.CapVision}},
		{ID: "c", ProviderID: "p1", Name: "m3"},
	}}
	res, err := ReconcileProviderCapabilities(context.Background(), providers, models,
		domain.Provider{ID: "p1", Name: "acme", Kind: domain.ProviderOpenAICompatible, BaseURL: "https://acme.test/v1"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Filled || res.Reason != ReconcileFilled {
		t.Fatalf("result = %+v, want filled", res)
	}
	if res.ModelsCounted != 2 {
		t.Errorf("counted = %d, want 2 (the cap-less model contributes nothing)", res.ModelsCounted)
	}
	for _, want := range []domain.Capability{domain.CapChat, domain.CapTools, domain.CapVision} {
		found := false
		for _, got := range res.Capabilities {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("union %v is missing %q", res.Capabilities, want)
		}
	}
	if got := providers.rows["p1"].Capabilities; len(got) != 3 {
		t.Errorf("stored provider caps = %v, want the union written", got)
	}
}

// The load-bearing rule, mirroring model detection: an operator's provider
// list is final and never second-guessed by a re-sync.
func TestReconcileNeverTouchesDeclaredProvider(t *testing.T) {
	declared := []domain.Capability{domain.CapChat}
	providers := newMemProviderStore(domain.Provider{
		ID: "p1", Name: "acme", Kind: domain.ProviderOpenAICompatible, BaseURL: "https://acme.test/v1",
		Capabilities: declared,
	})
	models := &memModelStore{rows: []domain.Model{
		{ID: "a", ProviderID: "p1", Name: "m1", Capabilities: []domain.Capability{domain.CapChat, domain.CapTools}},
	}}
	res, err := ReconcileProviderCapabilities(context.Background(), providers, models,
		domain.Provider{ID: "p1", Name: "acme", Capabilities: declared})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Filled || res.Reason != ReconcileDeclared {
		t.Fatalf("result = %+v, want untouched declared", res)
	}
	if got := providers.rows["p1"].Capabilities; len(got) != 1 {
		t.Errorf("declared provider was rewritten: %v", got)
	}
}

// No evidence anywhere: the row stays empty (kind default keeps applying)
// rather than writing a guess.
func TestReconcileLeavesUnknownAlone(t *testing.T) {
	providers := newMemProviderStore(domain.Provider{ID: "p1", Name: "acme"})
	models := &memModelStore{rows: []domain.Model{
		{ID: "a", ProviderID: "p1", Name: "m1"},
	}}
	res, err := ReconcileProviderCapabilities(context.Background(), providers, models,
		domain.Provider{ID: "p1", Name: "acme"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.Filled || res.Reason != ReconcileNothingLearned {
		t.Fatalf("result = %+v, want nothing_learned", res)
	}
	if len(providers.rows["p1"].Capabilities) != 0 {
		t.Errorf("empty union must not be written: %v", providers.rows["p1"].Capabilities)
	}
}

// Chat is required on every request by the engine, so a union without it
// would unroute the provider from all traffic — worse than leaving it empty.
func TestReconcileUnionAlwaysCarriesChat(t *testing.T) {
	providers := newMemProviderStore(domain.Provider{ID: "p1", Name: "acme"})
	models := &memModelStore{rows: []domain.Model{
		{ID: "a", ProviderID: "p1", Name: "m1", Capabilities: []domain.Capability{domain.CapTools}},
	}}
	res, err := ReconcileProviderCapabilities(context.Background(), providers, models,
		domain.Provider{ID: "p1", Name: "acme", Kind: domain.ProviderOpenAICompatible, BaseURL: "https://acme.test/v1"})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !res.Filled {
		t.Fatalf("result = %+v, want filled", res)
	}
	set := domain.NewCapabilitySet(res.Capabilities...)
	if !set.Contains(domain.CapChat) || !set.Contains(domain.CapTools) {
		t.Errorf("union = %v, want chat plus the evidence", res.Capabilities)
	}
}
