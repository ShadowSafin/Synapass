// Package benchmark implements the Autonomous Benchmark Engine.
//
// The engine is a fully independent, enterprise-grade module that continuously
// benchmarks providers and models, stores every measurement permanently, and
// powers a Grafana-style analytics dashboard.
//
// Architecture:
//
//	Dashboard → Benchmark API → Scheduler → Redis Queue → Workers → Gateway → Providers → Database
//
// The engine is DISABLED by default (config.Benchmark.Enabled=false) and must
// never affect gateway performance unless explicitly enabled. When disabled no
// scheduler ticks, no workers run, no queue traffic flows, and every API route
// reports the feature as disabled.
//
// The package is self-contained so it can later be extracted into its own
// service or sold as a premium enterprise feature: it only depends on config,
// domain and storage, plus a caller interface the gateway wires to its live
// provider adapters.
package benchmark
