"""Prometheus metrics.

``prometheus_client`` is imported lazily and its absence is not an error. The
workers must run on a bare interpreter -- that is what makes the scoring and
analysis logic testable without an install -- so every counter here degrades to a
no-op recorder when the library is missing.

Metric names follow the same ``synapass_`` prefix and ``_total``/``_seconds``
suffix conventions as the Go control plane, so one scrape config and one Grafana
dashboard can cover both processes. Label cardinality is bounded deliberately: no
request ids, no free-form model names from unknown sources.
"""

from __future__ import annotations

import logging
from collections.abc import Mapping
from typing import Any

logger = logging.getLogger(__name__)

# Namespace shared with the Go control plane's metrics.
NAMESPACE = "synapass"


class NullMetrics:
    """No-op metrics recorder used when ``prometheus_client`` is unavailable."""

    enabled = False

    def increment(self, name: str, value: float = 1.0, **labels: Any) -> None:
        return None

    def observe(self, name: str, value: float, **labels: Any) -> None:
        return None

    def set(self, name: str, value: float, **labels: Any) -> None:
        return None

    def start_server(self, addr: str) -> None:
        return None


class Metrics:
    """Prometheus-backed metrics recorder."""

    enabled = True

    def __init__(self) -> None:
        from prometheus_client import CollectorRegistry, Counter, Gauge, Histogram

        self._registry = CollectorRegistry()
        self._counters: dict[tuple[str, tuple[str, ...]], Any] = {}
        self._gauges: dict[tuple[str, tuple[str, ...]], Any] = {}
        self._histograms: dict[tuple[str, tuple[str, ...]], Any] = {}
        self._counter_cls = Counter
        self._gauge_cls = Gauge
        self._histogram_cls = Histogram

    # -- recording ---------------------------------------------------------

    def increment(self, name: str, value: float = 1.0, **labels: Any) -> None:
        counter = self._counter(name, sorted(labels))
        try:
            if labels:
                counter.labels(**labels).inc(value)
            else:
                counter.inc(value)
        except (ValueError, KeyError) as exc:
            # A label mismatch is a programming error. It is logged rather than
            # raised: losing a metric must never fail a job.
            logger.warning("failed to record counter %s: %s", name, exc)

    def observe(self, name: str, value: float, **labels: Any) -> None:
        histogram = self._histogram(name, sorted(labels))
        try:
            if labels:
                histogram.labels(**labels).observe(value)
            else:
                histogram.observe(value)
        except (ValueError, KeyError) as exc:
            logger.warning("failed to record histogram %s: %s", name, exc)

    def set(self, name: str, value: float, **labels: Any) -> None:
        gauge = self._gauge(name, sorted(labels))
        try:
            if labels:
                gauge.labels(**labels).set(value)
            else:
                gauge.set(value)
        except (ValueError, KeyError) as exc:
            logger.warning("failed to record gauge %s: %s", name, exc)

    # -- registration ------------------------------------------------------

    def _counter(self, name: str, label_names: list[str]) -> Any:
        key = (name, tuple(label_names))
        existing = self._counters.get(key)
        if existing is not None:
            return existing
        created = self._counter_cls(
            name, f"Synapass worker counter: {name}", label_names, registry=self._registry
        )
        self._counters[key] = created
        return created

    def _gauge(self, name: str, label_names: list[str]) -> Any:
        key = (name, tuple(label_names))
        existing = self._gauges.get(key)
        if existing is not None:
            return existing
        created = self._gauge_cls(
            name, f"Synapass worker gauge: {name}", label_names, registry=self._registry
        )
        self._gauges[key] = created
        return created

    def _histogram(self, name: str, label_names: list[str]) -> Any:
        key = (name, tuple(label_names))
        existing = self._histograms.get(key)
        if existing is not None:
            return existing
        created = self._histogram_cls(
            name,
            f"Synapass worker histogram: {name}",
            label_names,
            registry=self._registry,
        )
        self._histograms[key] = created
        return created

    # -- exposition --------------------------------------------------------

    def start_server(self, addr: str) -> None:
        """Serve ``/metrics`` on ``addr`` in a background thread."""
        from prometheus_client import start_http_server

        host, port = _split_addr(addr)
        start_http_server(port, addr=host, registry=self._registry)
        logger.info("prometheus metrics served", extra={"addr": addr})

    def render(self) -> str:
        """Render the current exposition text, used by the health endpoint."""
        from prometheus_client import generate_latest

        return generate_latest(self._registry).decode("utf-8", errors="replace")


def _split_addr(addr: str) -> tuple[str, int]:
    """Split ``host:port``, defaulting the host to all interfaces."""
    host, _, port = addr.rpartition(":")
    if not port:
        raise ValueError(f"metrics address {addr!r} must be host:port")
    try:
        return host or "0.0.0.0", int(port)
    except ValueError as exc:
        raise ValueError(f"metrics port in {addr!r} is not a number") from exc


def build(enabled: bool) -> NullMetrics | Metrics:
    """Construct a recorder, falling back to a no-op when unavailable."""
    if not enabled:
        return NullMetrics()
    try:
        return Metrics()
    except ImportError:
        logger.warning(
            "prometheus_client is not installed; worker metrics are disabled. "
            "Install it with: pip install prometheus-client"
        )
        return NullMetrics()


# ---------------------------------------------------------------------------
# Metric names
#
# Declared as constants so a dashboard query and the code that records it cannot
# drift: renaming a metric becomes a single edit rather than a silent gap in a
# graph.
# ---------------------------------------------------------------------------

METRIC_JOBS_TOTAL = f"{NAMESPACE}_worker_jobs_total"
METRIC_JOB_SECONDS = f"{NAMESPACE}_worker_job_duration_seconds"
METRIC_JOB_ERRORS_TOTAL = f"{NAMESPACE}_worker_job_errors_total"
METRIC_USAGE_EVENTS_TOTAL = f"{NAMESPACE}_worker_usage_events_total"
METRIC_TOKENS_TOTAL = f"{NAMESPACE}_worker_tokens_total"
METRIC_COST_USD_TOTAL = f"{NAMESPACE}_worker_cost_usd_total"
METRIC_PROVIDER_REQUESTS_TOTAL = f"{NAMESPACE}_worker_provider_requests_total"
METRIC_PROVIDER_ERROR_RATE = f"{NAMESPACE}_worker_provider_error_rate"
METRIC_ROLLUP_PUBLISHED_TOTAL = f"{NAMESPACE}_worker_rollups_published_total"
METRIC_BUS_RECONNECTS_TOTAL = f"{NAMESPACE}_worker_bus_reconnects_total"
METRIC_ANALYSIS_FLAGS_TOTAL = f"{NAMESPACE}_worker_analysis_flags_total"


def record_job(metrics: NullMetrics | Metrics, kind: str, status: str, duration_seconds: float) -> None:
    """Record one handled job."""
    metrics.increment(METRIC_JOBS_TOTAL, kind=kind, status=status)
    metrics.observe(METRIC_JOB_SECONDS, duration_seconds, kind=kind)
    if status not in {"ok", "skipped"}:
        metrics.increment(METRIC_JOB_ERRORS_TOTAL, kind=kind, status=status)


def record_usage(metrics: NullMetrics | Metrics, event: Any) -> None:
    """Record one consumed usage event."""
    metrics.increment(
        METRIC_USAGE_EVENTS_TOTAL,
        provider=event.provider or "unknown",
        outcome=event.outcome or "unknown",
    )
    usage = event.usage
    if usage.prompt_tokens:
        metrics.increment(
            METRIC_TOKENS_TOTAL,
            float(usage.prompt_tokens),
            provider=event.provider or "unknown",
            kind="prompt",
        )
    if usage.completion_tokens:
        metrics.increment(
            METRIC_TOKENS_TOTAL,
            float(usage.completion_tokens),
            provider=event.provider or "unknown",
            kind="completion",
        )
    if event.cost_usd:
        metrics.increment(
            METRIC_COST_USD_TOTAL,
            event.cost_usd,
            tenant=event.tenant_id or "unknown",
        )


def record_analysis(metrics: NullMetrics | Metrics, flags: Mapping[str, Any] | list[str]) -> None:
    """Record prompt-analysis flags."""
    for flag in flags:
        metrics.increment(METRIC_ANALYSIS_FLAGS_TOTAL, flag=str(flag))
