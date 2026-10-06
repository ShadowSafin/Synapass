package admin

import (
	"context"
	"sync"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// DetectionOptions bounds one capability-detection run.
type DetectionOptions struct {
	// Timeout bounds a single probe request. Zero means 30 seconds.
	Timeout time.Duration
	// MaxTokens is the completion allowance per probe. Zero means 1.
	MaxTokens int
	// Probes selects which capabilities to test. Empty means the default set.
	Probes []domain.Capability
	// Concurrency caps simultaneous upstream calls. Zero means 2. Probing is
	// real billable traffic, so this stays small on purpose: a run must never
	// look like a burst to the provider's rate limiter.
	Concurrency int
	// MaxModels caps how many models one run touches. Zero means 20. A cap
	// exists because each model costs four upstream calls by default, and a
	// 57-model provider should not surprise anyone with a 228-call bill from
	// a single click.
	MaxModels int
	// Only caps which models are eligible, never the semantics. Empty means
	// every model with an empty capability list.
	Only []string
}

// DetectionReport says what one detection run proved.
type DetectionReport struct {
	ProviderID string `json:"provider_id"`
	// Attempted lists the models that were probed, in run order.
	Attempted []string `json:"attempted"`
	// Proven maps each model to the capabilities a call demonstrated.
	Proven map[string][]domain.Capability `json:"proven"`
	// Indeterminate lists models where nothing could be decided: the row is
	// untouched and may be retried later.
	Indeterminate []string `json:"indeterminate"`
	// Skipped counts models that already declared capabilities and were
	// therefore never probed.
	Skipped int `json:"skipped"`
	// ProviderCapabilitiesFilled carries the provider-wide union written by
	// the post-run reconciliation, when the provider declared nothing and
	// the models now prove something. Empty when the provider row was left
	// alone. Set by the caller after reconciliation, not by the run itself.
	ProviderCapabilitiesFilled []domain.Capability `json:"provider_capabilities_filled,omitempty"`
}

// DetectMissingCapabilities probes the stored models of one provider that
// declare no capabilities and persists what the probes prove.
//
// The three rules that keep it safe:
//
//  1. A model with a non-empty capability list is never probed and never
//     written. An operator's declaration is final.
//  2. Only affirmative evidence is written. A refusal or an indeterminate
//     failure changes nothing.
//  3. Everything is bounded: MaxModels, Concurrency, and a per-probe Timeout.
//     Unbounded detection against a large catalogue is how a free feature
//     becomes someone's incident.
func DetectMissingCapabilities(ctx context.Context, adapter TestAdapter, store ModelStore, provider domain.Provider, opts DetectionOptions) (*DetectionReport, error) {
	concurrency := opts.Concurrency
	if concurrency <= 0 {
		concurrency = 2
	}
	maxModels := opts.MaxModels
	if maxModels <= 0 {
		maxModels = 20
	}

	stored, err := store.ListByProvider(ctx, provider.ID)
	if err != nil {
		return nil, domain.NewError(domain.ErrCodeInternal,
			"listing stored models failed").Wrap(err)
	}

	// Eligibility is decided up front so the run is deterministic: exactly the
	// models that need detection, in registry order, capped.
	only := map[string]bool{}
	for _, name := range opts.Only {
		only[name] = true
	}
	var targets []domain.Model
	for _, m := range stored {
		if len(opts.Only) > 0 && !only[m.Name] {
			continue
		}
		if len(m.Capabilities) > 0 {
			continue
		}
		if !m.Status.Usable() {
			continue
		}
		// Image, video and audio models are not chat completions: a chat
		// probe against them is pure token waste, and a shape one happens
		// to accept would mislabel the row as chat.
		if nonChatModel(m.Name) {
			continue
		}
		targets = append(targets, m)
		if len(targets) >= maxModels {
			break
		}
	}

	report := &DetectionReport{
		ProviderID: provider.ID,
		Proven:     map[string][]domain.Capability{},
	}
	// Models with a declared list are the only true skips. Models excluded by
	// Only or by status are simply not eligible, not skipped declarations.
	for _, m := range stored {
		if len(m.Capabilities) > 0 {
			report.Skipped++
		}
	}

	if len(targets) == 0 {
		return report, nil
	}

	// Bounded fan-out. One slot per in-flight probe; a probe that fails takes
	// its slot with it, so a slow provider cannot pile up goroutines.
	sem := make(chan struct{}, concurrency)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, m := range targets {
		m := m
		mu.Lock()
		report.Attempted = append(report.Attempted, m.Name)
		mu.Unlock()

		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				report.Indeterminate = append(report.Indeterminate, m.Name)
				mu.Unlock()
				return
			}

			ref := domain.ModelRef{ProviderID: provider.ID, ProviderName: provider.Name, Model: m.Name}
			outcome := ProbeModelCapabilities(ctx, adapter, ref, m.Name, ProbeOptions{
				Timeout:   opts.Timeout,
				MaxTokens: opts.MaxTokens,
				Probes:    opts.Probes,
			})

			mu.Lock()
			defer mu.Unlock()
			if len(outcome.Proven) == 0 {
				report.Indeterminate = append(report.Indeterminate, m.Name)
				return
			}
			m.Capabilities = outcome.Proven
			m.Metadata = withSource(m.Metadata, CapabilitiesSourceProbed)
			m.UpdatedAt = domain.Now()
			if _, uerr := store.Upsert(ctx, &m); uerr == nil {
				report.Proven[m.Name] = outcome.Proven
			} else {
				report.Indeterminate = append(report.Indeterminate, m.Name)
			}
		}()
	}
	wg.Wait()
	return report, nil
}