package admin

import (
	"context"
	"testing"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// richAdapter is an adapter that publishes capability metadata alongside ids.
type richAdapter struct {
	*fakeAdapter
	models []providers.RemoteModel
}

func (r richAdapter) ListModelsWithCapabilities(context.Context) ([]providers.RemoteModel, error) {
	return r.models, nil
}

func yes(b bool) *bool { return &b }

// A newly discovered model must arrive knowing what the provider said it can
// do, not just its name.
func TestSyncModelsStoresPublishedCapabilities(t *testing.T) {
	adapter := richAdapter{
		fakeAdapter: &fakeAdapter{name: "acme", models: []string{"m1"}},
		models: []providers.RemoteModel{{
			Name:         "m1",
			Capabilities: []domain.Capability{domain.CapChat, domain.CapTools},
		}},
	}
	store := &memModelStore{}

	res, err := SyncModels(context.Background(), adapter, store, SyncOptions{
		ProviderID: "p1", Environment: domain.EnvTest,
	})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Created) != 1 {
		t.Fatalf("created = %v", res.Created)
	}
	if res.CapabilitiesFilled != 1 {
		t.Fatalf("capabilities filled = %d, want 1", res.CapabilitiesFilled)
	}

	row := store.rows[len(store.rows)-1]
	if len(row.Capabilities) != 2 || !row.CapabilitySet().Contains(domain.CapTools) {
		t.Fatalf("row capabilities = %v, want tools", row.Capabilities)
	}
	if got := row.Metadata[CapabilitiesSourceKey]; got != CapabilitiesSourceProvider {
		t.Errorf("capabilities_source = %q, want %q", got, CapabilitiesSourceProvider)
	}
}

// The reported bug: a model already in the registry with no capability list is
// unroutable for tools until something fills it in. A re-sync that can prove the
// capabilities must repair that.
func TestSyncModelsBackfillsCapabilitiesOnExistingRows(t *testing.T) {
	adapter := richAdapter{
		fakeAdapter: &fakeAdapter{name: "acme", models: []string{"m1"}},
		models: []providers.RemoteModel{{
			Name:         "m1",
			Capabilities: []domain.Capability{domain.CapChat, domain.CapTools},
		}},
	}
	store := &memModelStore{rows: []domain.Model{
		{ID: "x", ProviderID: "p1", Name: "m1"},
	}}

	res, err := SyncModels(context.Background(), adapter, store, SyncOptions{ProviderID: "p1"})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Created) != 0 || len(res.Skipped) != 1 {
		t.Fatalf("created=%v skipped=%v, want an untouched model", res.Created, res.Skipped)
	}
	if res.CapabilitiesFilled != 1 {
		t.Fatalf("capabilities filled = %d, want 1", res.CapabilitiesFilled)
	}

	repaired := store.rows[len(store.rows)-1]
	if !repaired.CapabilitySet().Contains(domain.CapTools) {
		t.Fatalf("existing row was not repaired: %v", repaired.Capabilities)
	}
	if repaired.Metadata[CapabilitiesSourceKey] != CapabilitiesSourceProvider {
		t.Errorf("capabilities_source = %q", repaired.Metadata[CapabilitiesSourceKey])
	}
}

// The rule that protects operators: a list somebody wrote down is never
// overwritten by whatever the catalogue claims this time.
func TestSyncModelsNeverOverwritesDeclaredCapabilities(t *testing.T) {
	declared := []domain.Capability{domain.CapChat, domain.CapVision} // deliberately no tools
	adapter := richAdapter{
		fakeAdapter: &fakeAdapter{name: "acme", models: []string{"m1"}},
		models: []providers.RemoteModel{{
			Name:         "m1",
			Capabilities: []domain.Capability{domain.CapChat, domain.CapTools},
		}},
	}
	store := &memModelStore{rows: []domain.Model{
		{ID: "x", ProviderID: "p1", Name: "m1", Capabilities: declared},
	}}

	res, err := SyncModels(context.Background(), adapter, store, SyncOptions{ProviderID: "p1"})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.CapabilitiesFilled != 0 {
		t.Errorf("filled = %d, want 0: a declared list is not the sync's to change", res.CapabilitiesFilled)
	}
	row := store.rows[len(store.rows)-1]
	if row.CapabilitySet().Contains(domain.CapTools) {
		t.Fatalf("declared capabilities were overwritten: %v", row.Capabilities)
	}
	if row.CapabilitySet().Contains(domain.CapVision) != true {
		t.Fatalf("declared capability lost: %v", row.Capabilities)
	}
}

// A provider that publishes nothing richer must keep working exactly as before,
// with no capability claims invented from silence.
func TestSyncModelsFallsBackToBareListing(t *testing.T) {
	adapter := &fakeAdapter{name: "acme", models: []string{"m1"}}
	store := &memModelStore{}

	res, err := SyncModels(context.Background(), adapter, store, SyncOptions{ProviderID: "p1"})
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(res.Created) != 1 || res.CapabilitiesFilled != 0 {
		t.Fatalf("created=%v filled=%d", res.Created, res.CapabilitiesFilled)
	}
	row := store.rows[len(store.rows)-1]
	if len(row.Capabilities) != 0 {
		t.Fatalf("capabilities invented from an id-only listing: %v", row.Capabilities)
	}
	if _, ok := row.Metadata[CapabilitiesSourceKey]; ok {
		t.Error("no source should be recorded when nothing was learned")
	}
}
