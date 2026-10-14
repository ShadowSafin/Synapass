package benchmark

import (
	"context"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Reports returns recent reports for a tenant.
func (e *Engine) Reports(ctx context.Context, tenantID string, limit int) ([]domain.BenchmarkReport, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	return e.repos.BenchmarkReports.List(ctx, tenantID, limit)
}

// ListDatasets returns datasets visible to a tenant, with prompts attached.
func (e *Engine) ListDatasets(ctx context.Context, tenantID string) ([]domain.BenchmarkDataset, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	datasets, err := e.repos.BenchmarkDatasets.List(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	for i := range datasets {
		if prompts, perr := e.repos.BenchmarkPrompts.ListByDataset(ctx, datasets[i].ID); perr == nil {
			datasets[i].Prompts = prompts
		}
	}
	return datasets, nil
}

// CreateDataset stores a custom dataset with its prompts.
func (e *Engine) CreateDataset(ctx context.Context, tenantID, name, description string, prompts []domain.BenchmarkPrompt) (*domain.BenchmarkDataset, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	if name == "" {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "dataset name is required")
	}
	ds, err := e.repos.BenchmarkDatasets.Upsert(ctx, &domain.BenchmarkDataset{
		TenantID: tenantID, Name: name, Description: description,
	})
	if err != nil {
		return nil, err
	}
	for i := range prompts {
		prompts[i].DatasetID = ds.ID
		if prompts[i].Category == "" {
			prompts[i].Category = domain.BenchmarkCatReasoning
		}
		if prompts[i].Difficulty == "" {
			prompts[i].Difficulty = domain.BenchmarkDifficultyMedium
		}
		if _, cerr := e.repos.BenchmarkPrompts.Create(ctx, &prompts[i]); cerr != nil {
			return nil, cerr
		}
	}
	ds.Prompts = prompts
	return ds, nil
}

// SeedDefaultDataset ensures the global default corpus exists.
func (e *Engine) SeedDefaultDataset(ctx context.Context) error {
	if e.repos == nil {
		return nil
	}
	existing, err := e.repos.BenchmarkDatasets.List(ctx, "")
	if err != nil {
		return err
	}
	for _, d := range existing {
		if d.IsDefault && d.Name == DefaultDatasetName {
			if prompts, perr := e.repos.BenchmarkPrompts.ListByDataset(ctx, d.ID); perr == nil && len(prompts) > 0 {
				return nil
			}
		}
	}
	ds, err := e.repos.BenchmarkDatasets.Upsert(ctx, &domain.BenchmarkDataset{
		Name: DefaultDatasetName, Description: "Built-in benchmark corpus covering all categories.", IsDefault: true,
	})
	if err != nil {
		return err
	}
	for _, p := range DefaultPrompts() {
		p.DatasetID = ds.ID
		_, _ = e.repos.BenchmarkPrompts.Create(ctx, &p)
	}
	return nil
}

// ListNotifications returns delivery rules for a tenant.
func (e *Engine) ListNotifications(ctx context.Context, tenantID string) ([]domain.BenchmarkNotification, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	return e.repos.BenchmarkNotifications.List(ctx, tenantID)
}

// UpsertNotification creates or updates a delivery rule.
func (e *Engine) UpsertNotification(ctx context.Context, tenantID string, channel domain.BenchmarkNotificationChannel, target string, events []string, enabled bool) (*domain.BenchmarkNotification, error) {
	if err := e.requireEnabled(); err != nil {
		return nil, err
	}
	switch channel {
	case domain.BenchmarkNotifyEmail, domain.BenchmarkNotifyDiscord, domain.BenchmarkNotifySlack:
	default:
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "channel must be email, discord or slack")
	}
	if target == "" {
		return nil, domain.NewError(domain.ErrCodeInvalidRequest, "notification target is required")
	}
	if len(events) == 0 {
		events = []string{"benchmark_completed", "provider_down", "budget_exceeded"}
	}
	return e.repos.BenchmarkNotifications.Upsert(ctx, &domain.BenchmarkNotification{
		TenantID: tenantID, Channel: channel, Target: target, Events: events, Enabled: enabled,
		CreatedAt: time.Now().UTC(),
	})
}
