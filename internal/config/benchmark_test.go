package config

import "testing"

// The Autonomous Benchmark Engine must stay off unless explicitly enabled:
// any default-on behaviour would silently tax the inference gateway.
func TestBenchmarkDisabledByDefault(t *testing.T) {
	cfg := Default()
	if cfg.Benchmark.Enabled {
		t.Error("Default().Benchmark.Enabled must be false")
	}
	if !cfg.Benchmark.SchedulerEnabled {
		t.Error("the scheduler sub-switch defaults on (it only matters when the engine is enabled)")
	}
	if cfg.Benchmark.Workers <= 0 {
		t.Error("a sane worker default must be configured even while disabled")
	}
}

func TestBenchmarkEnabledViaEnvironment(t *testing.T) {
	t.Setenv("ENABLE_BENCHMARK_ENGINE", "true")
	cfg := Default()
	if err := cfg.applyEnv(); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if !cfg.Benchmark.Enabled {
		t.Error("ENABLE_BENCHMARK_ENGINE=true must enable the engine")
	}
}

func TestBenchmarkNamespacedAlias(t *testing.T) {
	t.Setenv("SYNAPASS_BENCHMARK_ENABLED", "true")
	cfg := Default()
	if err := cfg.applyEnv(); err != nil {
		t.Fatalf("applyEnv: %v", err)
	}
	if !cfg.Benchmark.Enabled {
		t.Error("SYNAPASS_BENCHMARK_ENABLED=true must enable the engine")
	}
}

func TestBenchmarkValidation(t *testing.T) {
	cfg := Default()
	cfg.Database.DSN = "postgres://u:p@localhost:5432/synapass"
	cfg.Benchmark.Workers = 500
	if err := cfg.Validate(); err == nil {
		t.Fatal("an absurd worker count must be rejected")
	}
}
