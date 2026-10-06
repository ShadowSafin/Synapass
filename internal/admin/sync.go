package admin

import (
	"context"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
	"github.com/shadowsafin/synapass/internal/providers"
)

// CapabilitiesSourceKey records in model metadata where a model's capability
// list came from, so an operator can tell a declared list from one the provider
// published. It is metadata rather than a column because nothing routes on it:
// it exists to answer "why does this model say it cannot do tools".
const CapabilitiesSourceKey = "capabilities_source"

// Source values for CapabilitiesSourceKey.
const (
	// CapabilitiesSourceProvider means the provider's own catalogue said so.
	CapabilitiesSourceProvider = "provider"
	// CapabilitiesSourceDeclared means an operator set the list by hand.
	CapabilitiesSourceDeclared = "declared"
)

// ModelStore is the persistence surface model sync needs. The storage model
// repository satisfies it unmodified; tests use an in-memory fake.
type ModelStore interface {
	ListByProvider(ctx context.Context, providerID string) ([]domain.Model, error)
	Upsert(ctx context.Context, m *domain.Model) (*domain.Model, error)
}

// SyncOptions configures one model sync run.
type SyncOptions struct {
	// ProviderID and ProviderName identify the sync subject.
	ProviderID   string
	ProviderName string
	// Environment is stamped on newly created rows, inheriting the provider's
	// environment so test fixtures do not leak into production routing.
	Environment domain.Environment
	// CreatedBy labels the run in history and audit.
	CreatedBy string
	// Timeout bounds the remote listing. Zero means 20 seconds.
	Timeout time.Duration
}

// SyncResult reports what one sync run did.
type SyncResult struct {
	ProviderID string   `json:"provider_id"`
	Created    []string `json:"created"`
	Skipped    []string `json:"skipped"`
	Total      int      `json:"total"`
	// CapabilitiesFilled counts models whose capability list this run supplied
	// from the provider's own catalogue.
	CapabilitiesFilled int `json:"capabilities_filled"`
	// CapabilitiesInferred counts models whose capability list this run
	// guessed from the family name. Zero-cost: no upstream call is sent.
	CapabilitiesInferred int `json:"capabilities_inferred"`
	// CapabilitySource records where model capabilities came from:
	// "provider" when the provider published them, "inferred" when only the
	// family-name guess supplied them, "ids_only" when its catalogue carried
	// nothing but names. Without this a filled count of zero is ambiguous
	// between "the provider told us nothing" and "everything was already
	// filled", which are very different things to be looking at.
	CapabilitySource string `json:"capability_source"`
}

// Capability catalogue sources, reported by SyncResult.CapabilitySource.
const (
	// CapabilitySourceProvider means the provider published per-model metadata.
	CapabilitySourceProvider = "provider"
	// CapabilitySourceInferred means only the zero-cost family-name guess
	// supplied capabilities.
	CapabilitySourceInferred = "inferred"
	// CapabilitySourceIDsOnly means the catalogue carried names only.
	CapabilitySourceIDsOnly = "ids_only"
)

// SyncModels discovers a provider's remote models and populates the registry.
//
// Every remote name absent from the registry becomes a model row owned by the
// API, carrying catalogue-published capabilities when the provider offers
// them or a family-name guess otherwise (zero upstream cost). Rows that
// already exist are left untouched, except that an inferred guess is upgraded
// when the catalogue now publishes metadata: a re-sync never reverts operator
// edits to pricing, aliases, status or priority, and never second-guesses a
// declared or probed list. Disabling or deleting a model therefore survives
// re-syncs, which is what makes the sync safe to run repeatedly.
func SyncModels(ctx context.Context, adapter TestAdapter, store ModelStore, opts SyncOptions) (*SyncResult, error) {
	lister, ok := adapter.(ModelLister)
	if !ok {
		return nil, domain.Errorf(domain.ErrCodeNotImplemented,
			"provider kind %q does not expose remote model listing", adapterKind(adapter))
	}
	if opts.Timeout <= 0 {
		opts.Timeout = 20 * time.Second
	}
	step, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()

	// Prefer a catalogue that carries capability metadata over one that carries
	// ids alone. The id listing is the only thing the OpenAI API defines, so
	// without this a model is created knowing nothing about what it can do and
	// falls back to the provider kind's default — which is how an OpenAI-
	// compatible server ends up marked as unable to call tools.
	catalogue, source, err := listCatalogue(step, adapter, lister)
	if err != nil {
		return nil, domain.NewError(domain.ErrCodeUpstream,
			"listing remote models failed").Wrap(err)
	}

	// The stored rows, kept whole rather than reduced to a name set: an existing
	// model with no capability list is a candidate for backfill, and that needs
	// the row itself.
	stored := map[string]domain.Model{}
	if store != nil {
		if have, lerr := store.ListByProvider(ctx, opts.ProviderID); lerr == nil {
			for _, m := range have {
				stored[strings.ToLower(m.Name)] = m
			}
		}
	}

	env := opts.Environment
	if env == "" {
		env = domain.EnvProduction
	}
	res := &SyncResult{ProviderID: opts.ProviderID, Total: len(catalogue), CapabilitySource: source}
	for _, remote := range catalogue {
		name := strings.TrimSpace(remote.Name)
		if name == "" {
			continue
		}
		prior, exists := stored[strings.ToLower(name)]
		if exists {
			res.Skipped = append(res.Skipped, name)
			// A model imported before the provider published capability metadata
			// has no list and is silently unroutable for tools. Fill it when the
			// catalogue now proves one, and only then: a list an operator wrote
			// (or a probe proved) is never second-guessed by a re-sync, which
			// is the same rule that protects their pricing and status. An
			// inferred guess sits below the catalogue in trust, so published
			// metadata replaces it.
			if len(remote.Capabilities) > 0 && store != nil &&
				(len(prior.Capabilities) == 0 || capabilitiesSource(prior) == CapabilitiesSourceInferred) {
				prior.Capabilities = remote.Capabilities
				prior.Metadata = withSource(prior.Metadata, CapabilitiesSourceProvider)
				prior.UpdatedAt = domain.Now()
				if _, uerr := store.Upsert(ctx, &prior); uerr == nil {
					res.CapabilitiesFilled++
				}
				continue
			}
			// No catalogue metadata and still no list: guess from the family
			// name. Zero-cost, and the row stays eligible for a later probe
			// or catalogue upgrade.
			if len(prior.Capabilities) == 0 && store != nil {
				if inferred := InferCapabilities(name); len(inferred) > 0 {
					prior.Capabilities = inferred
					prior.Metadata = withSource(prior.Metadata, CapabilitiesSourceInferred)
					prior.UpdatedAt = domain.Now()
					if _, uerr := store.Upsert(ctx, &prior); uerr == nil {
						res.CapabilitiesInferred++
					}
				}
			}
			continue
		}
		m := &domain.Model{
			ID:          domain.NewID(),
			ProviderID:  opts.ProviderID,
			Name:        name,
			DisplayName: name,
			Status:      domain.ModelActive,
			Priority:    100,
			Environment: env,
			ManagedBy:   domain.ManagedByAPI,
			CreatedAt:   domain.Now(),
			UpdatedAt:   domain.Now(),
		}
		if len(remote.Capabilities) > 0 {
			m.Capabilities = remote.Capabilities
			m.Metadata = withSource(nil, CapabilitiesSourceProvider)
			res.CapabilitiesFilled++
		} else if inferred := InferCapabilities(name); len(inferred) > 0 {
			// The catalogue carried a name only. Guess from the family
			// rather than leaving the row silent: zero upstream cost, and
			// a probe or published metadata can replace the guess later.
			m.Capabilities = inferred
			m.Metadata = withSource(nil, CapabilitiesSourceInferred)
			res.CapabilitiesInferred++
		}
		if store == nil {
			res.Created = append(res.Created, name)
			continue
		}
		if _, uerr := store.Upsert(ctx, m); uerr != nil {
			return nil, domain.NewError(domain.ErrCodeInternal,
				"store discovered model").Wrap(uerr)
		}
		// Mark it seen within this run: a catalogue that lists the same name
		// twice must create it once, and the store has not necessarily been
		// re-read.
		stored[strings.ToLower(name)] = *m
		res.Created = append(res.Created, name)
	}
	// The catalogue source describes the listing, not what the run achieved:
	// a names-only listing that still filled rows did so by inference.
	if res.CapabilitiesFilled > 0 {
		res.CapabilitySource = CapabilitySourceProvider
	} else if res.CapabilitiesInferred > 0 {
		res.CapabilitySource = CapabilitySourceInferred
	}
	return res, nil
}

// listCatalogue fetches the provider's models, preferring the view that carries
// capability metadata.
//
// A provider that publishes nothing richer than ids is the normal case, not a
// failure, so the bare listing is used silently and the caller sees the same
// result either way.
func listCatalogue(ctx context.Context, adapter TestAdapter, lister ModelLister) ([]providers.RemoteModel, string, error) {
	if rich, ok := adapter.(providers.ModelCapabilityLister); ok {
		if models, err := rich.ListModelsWithCapabilities(ctx); err == nil && len(models) > 0 {
			return models, CapabilitySourceProvider, nil
		}
	}
	names, err := lister.ListModels(ctx)
	if err != nil {
		return nil, CapabilitySourceIDsOnly, err
	}
	out := make([]providers.RemoteModel, 0, len(names))
	for _, n := range names {
		out = append(out, providers.RemoteModel{Name: n})
	}
	return out, CapabilitySourceIDsOnly, nil
}

// withSource records capability provenance without disturbing other metadata.
func withSource(existing map[string]string, source string) map[string]string {
	m := make(map[string]string, len(existing)+1)
	for k, v := range existing {
		m[k] = v
	}
	m[CapabilitiesSourceKey] = source
	return m
}

// adapterKind best-efforts the adapter kind for error messages.
func adapterKind(a TestAdapter) string {
	type kinder interface{ Kind() domain.ProviderKind }
	if k, ok := a.(kinder); ok {
		return string(k.Kind())
	}
	return a.Name()
}
