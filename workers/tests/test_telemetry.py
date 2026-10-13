"""Tests for the rolling telemetry aggregator."""

from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta

from synapass_workers.models import TokenUsage, UsageEvent
from synapass_workers.telemetry import Aggregator, Rollup, rollup_payload


class FakeClock:
    """A manually advanced clock so the window can be tested without sleeping."""

    def __init__(self) -> None:
        self.now = datetime(2026, 3, 15, 12, 0, 0, tzinfo=UTC)

    def __call__(self) -> datetime:
        return self.now

    def advance(self, seconds: float) -> None:
        self.now += timedelta(seconds=seconds)


def usage_event(
    *,
    provider: str = "openai",
    model: str = "gpt-4o",
    outcome: str = "success",
    latency_ms: int = 100,
    prompt: int = 10,
    completion: int = 5,
    cost: float = 0.001,
    estimated: bool = False,
    cache_hit: bool = False,
    tenant: str = "t-1",
) -> UsageEvent:
    return UsageEvent(
        provider=provider,
        model=model,
        outcome=outcome,
        latency_ms=latency_ms,
        provider_latency_ms=latency_ms - 5,
        cost_usd=cost,
        cache_hit=cache_hit,
        tenant_id=tenant,
        usage=TokenUsage(
            prompt_tokens=prompt,
            completion_tokens=completion,
            estimated=estimated,
        ),
    )


class TestIngestion(unittest.TestCase):
    def test_counts_requests_and_tokens(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(prompt=100, completion=50))

        snapshot = aggregator.snapshot()
        self.assertEqual(len(snapshot), 1)
        entry = snapshot[0]
        self.assertEqual(entry["provider"], "openai")
        self.assertEqual(entry["requests"], 1)
        self.assertEqual(entry["prompt_tokens"], 100)
        self.assertEqual(entry["completion_tokens"], 50)
        self.assertEqual(entry["total_tokens"], 150)
        self.assertAlmostEqual(entry["cost_usd"], 0.001)

    def test_groups_by_provider_and_model(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(provider="openai", model="gpt-4o"))
        aggregator.observe(usage_event(provider="openai", model="gpt-4o-mini"))
        aggregator.observe(usage_event(provider="anthropic", model="claude"))

        snapshot = aggregator.snapshot()
        self.assertEqual(len(snapshot), 3)
        # Ordering is deterministic so a dashboard table does not reshuffle.
        self.assertEqual(
            [(row["provider"], row["model"]) for row in snapshot],
            [("anthropic", "claude"), ("openai", "gpt-4o"), ("openai", "gpt-4o-mini")],
        )

    def test_missing_provider_is_bucketed_rather_than_dropped(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(provider="", model=""))
        snapshot = aggregator.snapshot()
        self.assertEqual(snapshot[0]["provider"], "unknown")
        self.assertEqual(snapshot[0]["model"], "unknown")

    def test_outcomes_partition_requests(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(outcome="success"))
        aggregator.observe(usage_event(outcome="fallback"))
        aggregator.observe(usage_event(outcome="error"))
        aggregator.observe(usage_event(outcome="rejected"))
        aggregator.observe(usage_event(outcome="canceled"))

        entry = aggregator.snapshot()[0]
        self.assertEqual(entry["requests"], 5)
        # A fallback is a success for the provider that answered, and is also
        # counted separately so a fallback storm stays visible.
        self.assertEqual(entry["successes"], 2)
        self.assertEqual(entry["fallbacks"], 1)
        self.assertEqual(entry["errors"], 1)
        self.assertEqual(entry["rejected"], 1)
        self.assertEqual(entry["canceled"], 1)

    def test_estimated_usage_is_counted_separately(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(estimated=True))
        aggregator.observe(usage_event(estimated=False))
        self.assertEqual(aggregator.snapshot()[0]["estimated_usage_requests"], 1)

    def test_cache_hits_are_counted(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(cache_hit=True))
        self.assertEqual(aggregator.snapshot()[0]["cache_hits"], 1)

    def test_derives_the_total_when_the_provider_omits_it(self) -> None:
        aggregator = Aggregator()
        event = UsageEvent(
            provider="openai", model="m", usage=TokenUsage(prompt_tokens=7, completion_tokens=3)
        )
        aggregator.observe(event)
        self.assertEqual(aggregator.snapshot()[0]["total_tokens"], 10)


class TestRates(unittest.TestCase):
    def test_error_rate_excludes_cancellations(self) -> None:
        # A client that closes the connection has not found a provider fault;
        # counting it would make impatient users look like an outage.
        rollup = Rollup()
        for _ in range(9):
            rollup.requests += 1
            rollup.successes += 1
        rollup.requests += 1
        rollup.canceled += 1
        self.assertEqual(rollup.error_rate, 0.0)

    def test_error_rate_is_errors_over_considered_requests(self) -> None:
        aggregator = Aggregator()
        for _ in range(8):
            aggregator.observe(usage_event(outcome="success"))
        for _ in range(2):
            aggregator.observe(usage_event(outcome="error"))
        self.assertAlmostEqual(aggregator.snapshot()[0]["error_rate"], 0.2)

    def test_success_and_fallback_rates(self) -> None:
        aggregator = Aggregator()
        for _ in range(3):
            aggregator.observe(usage_event(outcome="success"))
        aggregator.observe(usage_event(outcome="fallback"))

        entry = aggregator.snapshot()[0]
        self.assertEqual(entry["success_rate"], 1.0)
        self.assertAlmostEqual(entry["fallback_rate"], 0.25)

    def test_all_canceled_yields_a_zero_error_rate(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(outcome="canceled"))
        self.assertEqual(aggregator.snapshot()[0]["error_rate"], 0.0)


class TestLatency(unittest.TestCase):
    def test_average_and_percentiles_are_reported(self) -> None:
        aggregator = Aggregator()
        for latency in (10, 20, 30, 40, 50, 60, 70, 80, 90, 100):
            aggregator.observe(usage_event(latency_ms=latency))

        entry = aggregator.snapshot()[0]
        self.assertEqual(entry["average_latency_ms"], 55)
        self.assertEqual(entry["latency_p50_ms"], 50)
        self.assertEqual(entry["latency_p95_ms"], 100)

    def test_zero_latency_samples_are_not_recorded(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(latency_ms=0))
        entry = aggregator.snapshot()[0]
        self.assertEqual(entry["latency_p95_ms"], 0)

    def test_latency_samples_are_bounded(self) -> None:
        # The bound is what stops one very hot provider from growing the worker's
        # memory without limit between evictions.
        aggregator = Aggregator(max_samples=4)
        for latency in range(1, 101):
            aggregator.observe(usage_event(latency_ms=latency))

        rollup = aggregator._rollups[("openai", "gpt-4o")]
        self.assertEqual(len(rollup.latencies), 4)
        self.assertEqual(list(rollup.latencies), [97, 98, 99, 100])


class TestWindowing(unittest.TestCase):
    def test_stale_rollups_are_evicted(self) -> None:
        clock = FakeClock()
        aggregator = Aggregator(window_seconds=60, clock=clock)

        aggregator.observe(usage_event(provider="openai"))
        clock.advance(30)
        aggregator.observe(usage_event(provider="anthropic"))
        self.assertEqual(len(aggregator.snapshot()), 2)

        # Past the window, the idle provider disappears while the active one stays.
        clock.advance(40)
        aggregator.observe(usage_event(provider="anthropic"))
        providers = [row["provider"] for row in aggregator.snapshot()]
        self.assertEqual(providers, ["anthropic"])

    def test_expire_reports_how_many_were_dropped(self) -> None:
        clock = FakeClock()
        aggregator = Aggregator(window_seconds=10, clock=clock)
        aggregator.observe(usage_event(provider="a"))
        aggregator.observe(usage_event(provider="b"))

        clock.advance(60)
        self.assertEqual(aggregator.expire(), 2)
        self.assertEqual(aggregator.snapshot(), [])

    def test_totals_are_lifetime_not_windows(self) -> None:
        clock = FakeClock()
        aggregator = Aggregator(window_seconds=10, clock=clock)
        aggregator.observe(usage_event())

        clock.advance(600)
        aggregator.expire()

        totals = aggregator.totals()
        self.assertEqual(totals["requests"], 1)
        self.assertEqual(totals["observed_events"], 1)
        self.assertEqual(totals["active_keys"], 0)

    def test_totals_track_evictions(self) -> None:
        clock = FakeClock()
        aggregator = Aggregator(window_seconds=10, clock=clock)
        aggregator.observe(usage_event(provider="a"))
        clock.advance(60)
        aggregator.expire()
        self.assertEqual(aggregator.totals()["evictions"], 1)

    def test_rejects_a_non_positive_window(self) -> None:
        with self.assertRaises(ValueError):
            Aggregator(window_seconds=0)
        with self.assertRaises(ValueError):
            Aggregator(max_samples=0)


class TestProviderHealth(unittest.TestCase):
    def test_rolls_models_up_to_one_row_per_provider(self) -> None:
        aggregator = Aggregator()
        aggregator.observe(usage_event(provider="openai", model="gpt-4o"))
        aggregator.observe(usage_event(provider="openai", model="gpt-4o-mini"))
        aggregator.observe(usage_event(provider="anthropic", model="claude"))

        health = aggregator.provider_health()
        self.assertEqual([row["provider"] for row in health], ["anthropic", "openai"])
        openai = health[1]
        self.assertEqual(openai["models"], 2)
        self.assertEqual(openai["requests"], 2)

    def test_no_verdict_is_issued_below_the_sample_floor(self) -> None:
        # One failure out of one request is not evidence of a degraded provider;
        # the router treats unknown as eligible, which keeps it in rotation.
        aggregator = Aggregator()
        aggregator.observe(usage_event(outcome="error"))
        self.assertEqual(aggregator.provider_health()[0]["state"], "unknown")

    def test_error_rate_drives_the_state(self) -> None:
        healthy = Aggregator()
        for _ in range(20):
            healthy.observe(usage_event(outcome="success"))
        self.assertEqual(healthy.provider_health()[0]["state"], "healthy")

        degraded = Aggregator()
        for _ in range(15):
            degraded.observe(usage_event(outcome="success"))
        for _ in range(5):
            degraded.observe(usage_event(outcome="error"))
        self.assertEqual(degraded.provider_health()[0]["state"], "degraded")

        unhealthy = Aggregator()
        for _ in range(5):
            unhealthy.observe(usage_event(outcome="success"))
        for _ in range(15):
            unhealthy.observe(usage_event(outcome="error"))
        self.assertEqual(unhealthy.provider_health()[0]["state"], "unhealthy")

    def test_cancellations_do_not_push_a_provider_to_unhealthy(self) -> None:
        aggregator = Aggregator()
        for _ in range(20):
            aggregator.observe(usage_event(outcome="canceled"))
        self.assertEqual(aggregator.provider_health()[0]["state"], "unknown")

    def test_provider_health_reports_costs_and_latency(self) -> None:
        aggregator = Aggregator()
        for latency in (10, 20, 30):
            aggregator.observe(usage_event(latency_ms=latency, cost=0.5))

        row = aggregator.provider_health()[0]
        self.assertAlmostEqual(row["cost_usd"], 1.5)
        self.assertEqual(row["average_latency_ms"], 20)
        self.assertEqual(row["latency_p50_ms"], 20)
        self.assertNotIn("latencies", row)


class TestRollupPayload(unittest.TestCase):
    def test_payload_carries_totals_providers_and_models(self) -> None:
        aggregator = Aggregator(window_seconds=120)
        aggregator.observe(usage_event())

        payload = rollup_payload(aggregator)
        self.assertEqual(payload["window_seconds"], 120)
        self.assertIn("totals", payload)
        self.assertIn("providers", payload)
        self.assertIn("models", payload)
        self.assertTrue(payload["generated_at"])


class TestObserveMany(unittest.TestCase):
    def test_returns_the_number_accepted(self) -> None:
        aggregator = Aggregator()
        count = aggregator.observe_many([usage_event(), usage_event()])
        self.assertEqual(count, 2)
        self.assertEqual(aggregator.totals()["requests"], 2)


if __name__ == "__main__":
    unittest.main()
