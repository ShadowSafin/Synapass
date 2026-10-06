package admin

import (
	"context"
	"sort"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ProviderStore is the persistence surface provider reconciliation needs. The
// storage provider repository satisfies it unmodified; tests use an in-memory
// fake.
type ProviderStore interface {
	GetByID(ctx context.Context, id string) (*domain.Provider, error)
	Upsert(ctx context.Context, p *domain.Provider) (*domain.Provider, error)
}

// Reconcile reasons, reported so an unchanged provider is distinguishable
// from a failure: "nothing happened" must not look like "nothing was learned".
const (
	// ReconcileDeclared means the provider already lists capabilities, so an
	// operator's (or a previous run's) declaration is final and untouched.
	ReconcileDeclared = "declared"
	// ReconcileFilled means the union was written to the provider row.
	ReconcileFilled = "filled"
	// ReconcileNothingLearned means no stored model declares anything yet, so
	// there is nothing to union. Probing (sync auto-detect or the explicit
	// endpoint) is what produces the evidence this step consumes.
	ReconcileNothingLearned = "nothing_learned"
)

// ReconcileResult reports what one provider-capability reconciliation did.
type ReconcileResult struct {
	ProviderID string `json:"provider_id"`
	// Filled is true when the provider row was written.
	Filled bool `json:"filled"`
	// Capabilities is the union written, or the existing declaration when the
	// provider already had one.
	Capabilities []domain.Capability `json:"capabilities"`
	// Reason is one of declared, filled or nothing_learned.
	Reason string `json:"reason"`
	// ModelsCounted is how many stored models contributed to the union.
	ModelsCounted int `json:"models_counted"`
}

// ReconcileProviderCapabilities unions stored model capabilities into the
// provider row when the provider declares none.
//
// This is the step that makes model sync actually fix routing: the routing
// engine gates every request on the provider-wide list first, so models that
// learned (or were published with) tools change nothing while the provider
// row stays empty and the kind default keeps saying "no tools".
//
// The three rules that keep it safe, mirroring model detection:
//
//  1. A provider with a non-empty list is never touched. An operator's
//     declaration is final.
//  2. Only stored evidence is unioned. Nothing is probed here; this function
//     spends no upstream tokens.
//  3. Chat is always included in a non-empty union. The engine requires chat
//     on every request, so writing a union without it would unroute the
//     provider from everything, which is worse than leaving it empty.
func ReconcileProviderCapabilities(ctx context.Context, providers ProviderStore, models ModelStore, provider domain.Provider) (*ReconcileResult, error) {
	if len(provider.Capabilities) > 0 {
		return &ReconcileResult{
			ProviderID:   provider.ID,
			Capabilities: provider.Capabilities,
			Reason:       ReconcileDeclared,
		}, nil
	}

	var stored []domain.Model
	if models != nil {
		have, err := models.ListByProvider(ctx, provider.ID)
		if err != nil {
			return nil, domain.NewError(domain.ErrCodeInternal,
				"listing stored models failed").Wrap(err)
		}
		stored = have
	}

	set := domain.NewCapabilitySet()
	counted := 0
	for _, m := range stored {
		if len(m.Capabilities) == 0 {
			continue
		}
		counted++
		for _, c := range m.Capabilities {
			if c != "" {
				set[c] = struct{}{}
			}
		}
	}
	if len(set) == 0 {
		return &ReconcileResult{ProviderID: provider.ID, Reason: ReconcileNothingLearned}, nil
	}
	// Every request requires chat; a union without it would unroute the
	// provider from all traffic. Model rows learned by probing always carry
	// it, so this only fires for catalogue-published lists that omitted it.
	set[domain.CapChat] = struct{}{}

	union := make([]domain.Capability, 0, len(set))
	for c := range set {
		union = append(union, c)
	}
	sort.Slice(union, func(i, j int) bool { return union[i] < union[j] })

	provider.Capabilities = union
	if err := ValidateProvider(&provider); err != nil {
		return nil, err
	}
	if providers == nil {
		return &ReconcileResult{
			ProviderID:    provider.ID,
			Filled:        true,
			Capabilities:  union,
			Reason:        ReconcileFilled,
			ModelsCounted: counted,
		}, nil
	}
	if _, err := providers.Upsert(ctx, &provider); err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal,
			"store reconciled provider capabilities").Wrap(err)
	}
	return &ReconcileResult{
		ProviderID:    provider.ID,
		Filled:        true,
		Capabilities:  union,
		Reason:        ReconcileFilled,
		ModelsCounted: counted,
	}, nil
}
