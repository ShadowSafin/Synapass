package storage

import (
	"strings"
	"testing"
)

// The benchmark migration must embed cleanly, split into statements, and
// create every table the engine's repositories touch. A missing table here
// would surface as a runtime failure only when the feature is enabled, which
// is exactly when it hurts most.
func TestBenchmarkMigrationIsComplete(t *testing.T) {
	migrations, err := PostgresMigrations()
	if err != nil {
		t.Fatalf("PostgresMigrations: %v", err)
	}
	var found *Migration
	for i := range migrations {
		if migrations[i].Version == "0013" {
			found = &migrations[i]
		}
	}
	if found == nil {
		t.Fatal("migration 0013_benchmark is missing from the embedded set")
	}
	stmts := SplitStatements(found.SQL)
	if len(stmts) == 0 {
		t.Fatal("migration 0013 splits into zero statements")
	}
	for _, table := range []string{
		"benchmark_jobs", "benchmark_runs", "benchmark_results", "benchmark_metrics",
		"benchmark_datasets", "benchmark_prompts", "benchmark_reports",
		"benchmark_notifications", "benchmark_worker_logs",
	} {
		if !strings.Contains(found.SQL, table) {
			t.Errorf("migration 0013 does not mention %s", table)
		}
	}
	// Ordering: versions must be strictly ascending so the runner applies 0013 last.
	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version <= migrations[i-1].Version {
			t.Errorf("migrations out of order: %s after %s", migrations[i].Version, migrations[i-1].Version)
		}
	}
}
