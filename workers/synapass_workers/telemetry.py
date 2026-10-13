"""Rolling telemetry aggregation.

The control plane writes every request to ClickHouse, which answers historical
questions well and "what is happening to provider X in the last five minutes"
badly: a dashboard doing that in SQL re-scans an ever-growing table on every
refresh. This consumer keeps a small in-memory window instead and republishes
rollups, which is what the provider health panels and the routing latency priors
consume.

# Bounds

The window is bounded in two independent ways, because either alone is
insufficient:

* **time** -- observations older than :attr:`Aggregator.window_seconds` are
  dropped, so a quiet period does not leave stale numbers looking current.
* **count** -- at most :attr:`Aggregator.max_samples` latency samples are kept per
  key, so one very hot provider cannot grow unbounded between evictions.

Latency samples are bounded; counters are not. A counter is eight bytes and must
survive the window, whereas a sample list is what would actually exhaust memory.
"""

from __future__ import annotations

import threading
from collections import deque
from collections.abc import Iterable, Mapping
from dataclasses import dataclass, field
from datetime import UTC, datetime, timedelta
from typing import Any

from .models import UsageEvent
from .scoring import percentile


@dataclass
class Rollup:
    """Counters and latency samples for one provider/model pair."""

    provider: str = ""
    model: str = ""
    requests: int = 0
    successes: int = 0
    errors: int = 0
    rejected: int = 0
    canceled: int = 0
    fallbacks: int = 0
    cache_hits: int = 0
    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0
    cost_usd: float = 0.0
    latency_sum_ms: int = 0
    provider_latency_sum_ms: int = 0
    estimated_usage_requests: int = 0
    latencies: deque[int] = field(default_factory=lambda: deque(maxlen=2048))
    last_seen: datetime = field(default_factory=lambda: datetime.now(UTC))

    @property
    def average_latency_ms(self) -> int:
        if self.requests == 0:
            return 0
        return int(self.latency_sum_ms / self.requests)

    @property
    def error_rate(self) -> float:
        if self.requests == 0:
            return 0.0
        # Cancellations are excluded from the denominator of the error rate: a
        # client that closes the connection has not discovered a provider fault,
        # and counting it would make a scikit of impatient users look like an
        # outage.
        considered = self.requests - self.canceled
        if considered <= 0:
            return 0.0
        return round(self.errors / considered, 6)

    @property
    def fallback_rate(self) -> float:
        if self.requests == 0:
            return 0.0
        return round(self.fallbacks / self.requests, 6)

    @property
    def success_rate(self) -> float:
        if self.requests == 0:
            return 0.0
        return round(self.successes / self.requests, 6)

    def snapshot(self) -> dict[str, Any]:
        return {
            "provider": self.provider,
            "model": self.model,
            "requests": self.requests,
            "successes": self.successes,
            "errors": self.errors,
            "rejected": self.rejected,
            "canceled": self.canceled,
            "fallbacks": self.fallbacks,
            "cache_hits": self.cache_hits,
            "prompt_tokens": self.prompt_tokens,
            "completion_tokens": self.completion_tokens,
            "total_tokens": self.total_tokens,
            "cost_usd": round(self.cost_usd, 8),
            "average_latency_ms": self.average_latency_ms,
            "latency_p50_ms": int(percentile(self.latencies, 50)),
            "latency_p95_ms": int(percentile(self.latencies, 95)),
            "latency_p99_ms": int(percentile(self.latencies, 99)),
            "success_rate": self.success_rate,
            "error_rate": self.error_rate,
            "fallback_rate": self.fallback_rate,
            "estimated_usage_requests": self.estimated_usage_requests,
            "last_seen": self.last_seen.astimezone(UTC).isoformat(),
        }


class Aggregator:
    """Thread-safe rolling aggregate over usage events.

    The lock is a plain mutex rather than something cleverer because the critical
    section is a handful of integer increments: contention is irrelevant next to
    the cost of getting a counter wrong.
    """

    def __init__(
        self,
        window_seconds: int = 300,
        max_samples: int = 2048,
        clock: Any = None,
    ) -> None:
        if window_seconds <= 0:
            raise ValueError("window_seconds must be positive")
        if max_samples <= 0:
            raise ValueError("max_samples must be positive")
        self.window_seconds = window_seconds
        self.max_samples = max_samples
        self._clock = clock or (lambda: datetime.now(UTC))
        self._lock = threading.Lock()
        self._rollups: dict[tuple[str, str], Rollup] = {}
        # Lifetime totals also carry a bounded sample list, so the grand-total
        # percentile is over the most recent window's worth of samples rather than
        # over every sample ever seen.
        self._totals = Rollup(provider="", model="", latencies=deque(maxlen=max_samples))
        self._observed = 0
        self._evictions = 0

    # -- ingestion ---------------------------------------------------------

    def observe(self, event: UsageEvent) -> Rollup:
        """Record one usage event, returning the updated rollup."""
        now = self._clock()
        key = (event.provider or "unknown", event.model or "unknown")

        with self._lock:
            self._evict_locked(now)
            rollup = self._rollups.get(key)
            if rollup is None:
                rollup = Rollup(
                    provider=key[0],
                    model=key[1],
                    latencies=deque(maxlen=self.max_samples),
                )
                self._rollups[key] = rollup

            _apply(rollup, event, now)
            _apply(self._totals, event, now)
            self._observed += 1
            return rollup

    def observe_many(self, events: Iterable[UsageEvent]) -> int:
        """Record several events, returning how many were accepted."""
        count = 0
        for event in events:
            self.observe(event)
            count += 1
        return count

    # -- eviction ----------------------------------------------------------

    def _evict_locked(self, now: datetime) -> None:
        """Drop rollups whose newest observation has aged out of the window."""
        cutoff = now - timedelta(seconds=self.window_seconds)
        stale = [key for key, rollup in self._rollups.items() if rollup.last_seen < cutoff]
        for key in stale:
            del self._rollups[key]
            self._evictions += 1

        # Totals are a lifetime counter, not a window, so they are never evicted.
        # They exist to answer "how much has this worker seen", which is a
        # different question from "what is hot right now".

    def expire(self, now: datetime | None = None) -> int:
        """Force eviction, returning how many rollups were dropped."""
        with self._lock:
            before = len(self._rollups)
            self._evict_locked(now or self._clock())
            return before - len(self._rollups)

    # -- reporting ---------------------------------------------------------

    def snapshot(self) -> list[dict[str, Any]]:
        """Render per-provider rollups, ordered deterministically."""
        with self._lock:
            items = [rollup.snapshot() for rollup in self._rollups.values()]
        items.sort(key=lambda item: (item["provider"], item["model"]))
        return items

    def totals(self) -> dict[str, Any]:
        """Render lifetime totals."""
        with self._lock:
            snapshot = self._totals.snapshot()
            snapshot["observed_events"] = self._observed
            snapshot["active_keys"] = len(self._rollups)
            snapshot["evictions"] = self._evictions
        return snapshot

    def provider_health(self) -> list[dict[str, Any]]:
        """Summarise per-provider health across every model on that provider.

        The router consumes this shape: an operator-facing health view wants one
        row per provider, not one per provider/model pair.
        """
        with self._lock:
            grouped: dict[str, dict[str, Any]] = {}
            for rollup in self._rollups.values():
                entry = grouped.setdefault(
                    rollup.provider,
                    {
                        "provider": rollup.provider,
                        "models": 0,
                        "requests": 0,
                        "successes": 0,
                        "errors": 0,
                        "canceled": 0,
                        "fallbacks": 0,
                        "cost_usd": 0.0,
                        "latency_sum_ms": 0,
                        "latencies": [],
                    },
                )
                entry["models"] += 1
                entry["requests"] += rollup.requests
                entry["successes"] += rollup.successes
                entry["errors"] += rollup.errors
                entry["canceled"] += rollup.canceled
                entry["fallbacks"] += rollup.fallbacks
                entry["cost_usd"] += rollup.cost_usd
                entry["latency_sum_ms"] += rollup.latency_sum_ms
                entry["latencies"].extend(rollup.latencies)

        out: list[dict[str, Any]] = []
        for entry in grouped.values():
            latencies = entry.pop("latencies")
            requests = entry["requests"]
            considered = max(requests - entry["canceled"], 0)
            entry["latency_p50_ms"] = int(percentile(latencies, 50))
            entry["latency_p95_ms"] = int(percentile(latencies, 95))
            entry["latency_p99_ms"] = int(percentile(latencies, 99))
            entry["average_latency_ms"] = int(entry["latency_sum_ms"] / requests) if requests else 0
            entry["success_rate"] = round(entry["successes"] / requests, 6) if requests else 0.0
            entry["error_rate"] = round(entry["errors"] / considered, 6) if considered else 0.0
            entry["fallback_rate"] = round(entry["fallbacks"] / requests, 6) if requests else 0.0
            entry["cost_usd"] = round(entry["cost_usd"], 8)
            entry["state"] = _health_state(entry["error_rate"], considered)
            out.append(entry)

        out.sort(key=lambda item: item["provider"])
        return out


def _apply(rollup: Rollup, event: UsageEvent, now: datetime) -> None:
    """Fold one event into a rollup.

    A module-level function rather than a method so it cannot accidentally read or
    write aggregator state, which keeps it a pure fold that is easy to reason
    about and to reuse.
    """
    rollup.requests += 1
    rollup.last_seen = now

    outcome = (event.outcome or "").lower()
    if outcome == "success":
        rollup.successes += 1
    elif outcome == "fallback":
        # A served fallback is a success for the provider that answered; it is
        # tracked separately so a fallback storm stays visible.
        rollup.successes += 1
        rollup.fallbacks += 1
    elif outcome == "rejected":
        rollup.rejected += 1
    elif outcome == "canceled":
        rollup.canceled += 1
    else:
        rollup.errors += 1

    if event.cache_hit:
        rollup.cache_hits += 1

    usage = event.usage
    rollup.prompt_tokens += usage.prompt_tokens
    rollup.completion_tokens += usage.completion_tokens
    rollup.total_tokens += usage.effective_total
    rollup.cost_usd += event.cost_usd
    if usage.estimated:
        rollup.estimated_usage_requests += 1

    rollup.latency_sum_ms += event.latency_ms
    rollup.provider_latency_sum_ms += event.provider_latency_ms
    if event.latency_ms > 0 and rollup.latencies.maxlen:
        # The deque discards from the left once it is full, which is what keeps a
        # single very hot provider from growing without bound between evictions.
        rollup.latencies.append(event.latency_ms)


def _health_state(error_rate: float, considered: int) -> str:
    """Map an observed error rate onto the health vocabulary the router uses.

    A single request that failed is not evidence of a degraded provider, so no
    verdict is issued below a small sample floor; the router then treats the
    provider as unknown, which keeps it eligible.
    """
    if considered < 10:
        return "unknown"
    if error_rate >= 0.5:
        return "unhealthy"
    if error_rate >= 0.1:
        return "degraded"
    return "healthy"


def rollup_payload(aggregator: Aggregator) -> Mapping[str, Any]:
    """Render the period summary published on ``ar.telemetry.rollup``."""
    return {
        "totals": aggregator.totals(),
        "providers": aggregator.provider_health(),
        "models": aggregator.snapshot(),
        "window_seconds": aggregator.window_seconds,
        "generated_at": datetime.now(UTC).isoformat(),
    }
