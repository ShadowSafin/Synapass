"""Worker runtime.

One process, two independent responsibilities:

* **Intelligence** -- consumes ``ar.eval.job`` and publishes a result per job.
* **Telemetry** -- consumes ``ar.usage.recorded`` and publishes periodic rollups.

They share a process because they share a deployment and a connection, and are
kept as separate subscriptions rather than merged because their failure modes
differ: a failing evaluation must not delay telemetry, and a burst of telemetry
must not starve an evaluation. Each has its own durable consumer name, so a
redelivery budget is spent per concern.
"""

from __future__ import annotations

import contextlib
import logging
import signal
import threading
import time
from collections.abc import Mapping
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any

from . import ENGINE_VERSION, __version__
from .bus import (
    DURABLE_INTELLIGENCE,
    DURABLE_TELEMETRY,
    SUBJECT_EVAL_JOB,
    SUBJECT_EVAL_RESULT,
    SUBJECT_PROMPT_ANALYSIS,
    SUBJECT_TELEMETRY_ROLLUP,
    SUBJECT_USAGE_RECORDED,
    BusError,
    NullBus,
    decode,
)
from .config import Config
from .jobs import UnknownJobKindError, dispatch, parse_job
from .metrics import (
    METRIC_BUS_RECONNECTS_TOTAL,
    METRIC_ROLLUP_PUBLISHED_TOTAL,
    record_analysis,
    record_job,
    record_usage,
)
from .metrics import (
    build as build_metrics,
)
from .models import KIND_PROMPT_ANALYSIS, STATUS_FAILED, STATUS_OK, UsageEvent
from .telemetry import Aggregator, rollup_payload

logger = logging.getLogger(__name__)


@dataclass
class RuntimeStats:
    """Counters exposed on the health endpoint."""

    jobs_handled: int = 0
    jobs_failed: int = 0
    usage_events: int = 0
    results_published: int = 0
    rollups_published: int = 0
    decode_failures: int = 0
    started_at: float = 0.0

    def uptime_seconds(self) -> float:
        if not self.started_at:
            return 0.0
        return round(time.time() - self.started_at, 3)

    def as_dict(self) -> dict[str, Any]:
        return {
            "jobs_handled": self.jobs_handled,
            "jobs_failed": self.jobs_failed,
            "usage_events": self.usage_events,
            "results_published": self.results_published,
            "rollups_published": self.rollups_published,
            "decode_failures": self.decode_failures,
            "uptime_seconds": self.uptime_seconds(),
        }


class Worker:
    """Runs the intelligence and telemetry consumers."""

    def __init__(
        self,
        config: Config,
        *,
        bus: Any | None = None,
        aggregator: Aggregator | None = None,
        metrics: Any | None = None,
    ) -> None:
        self.config = config
        self.bus = bus if bus is not None else NullBus()
        self.aggregator = aggregator or Aggregator(window_seconds=300)
        self.metrics = metrics if metrics is not None else build_metrics(config.metrics_enabled)
        self.stats = RuntimeStats()
        self._stop = threading.Event()
        self._rollup_thread: threading.Thread | None = None

    # -- lifecycle ---------------------------------------------------------

    def start(self) -> None:
        """Connect and begin consuming."""
        self.stats.started_at = time.time()

        if self.config.metrics_enabled:
            try:
                self.metrics.start_server(self.config.metrics_addr)
            except Exception as exc:
                # A metrics port conflict must not stop the worker from doing its
                # actual job, but it must be loud.
                logger.error("could not start the metrics server: %s", exc)

        try:
            self.bus.connect()
            self.bus.subscribe_jetstream(SUBJECT_EVAL_JOB, DURABLE_INTELLIGENCE, self.handle_job_message)
            self.bus.subscribe_jetstream(SUBJECT_USAGE_RECORDED, DURABLE_TELEMETRY, self.handle_usage_message)
        except Exception as exc:
            # A worker that cannot reach NATS stays up serving metrics and
            # reporting degraded health rather than crash-looping. The gateway
            # does not depend on workers for inference, so this is the correct
            # failure mode for verification: the stack is inspectable while the
            # broker problem is fixed.
            logger.error("nats unavailable; workers running degraded: %s", exc)
            with contextlib.suppress(Exception):
                self.metrics.increment("synapass_worker_bus_reconnects_total")

        self._rollup_thread = threading.Thread(target=self._rollup_loop, name="rollup-publisher", daemon=True)
        self._rollup_thread.start()
        logger.info(
            "worker started",
            extra={
                "version": __version__,
                "engine_version": ENGINE_VERSION,
                "judging_enabled": self.config.judging_enabled,
            },
        )

    def stop(self) -> None:
        """Signal shutdown and close the bus."""
        self._stop.set()
        if self._rollup_thread is not None:
            self._rollup_thread.join(timeout=10)
        self.bus.close()
        logger.info("worker stopped", extra=self.stats.as_dict())

    def run_forever(self) -> None:
        """Run until SIGINT or SIGTERM."""
        self.install_signal_handlers()
        self.start()

        # The main thread blocks here rather than polling: the subscriptions run on
        # the client's delivery threads, so there is nothing to drive.
        while not self._stop.wait(timeout=1.0):
            pass

        self.stop()

    def install_signal_handlers(self) -> None:
        """Translate a termination signal into a graceful stop.

        Signal handlers can only run on the main thread, and a library running the
        worker (a test harness, an embedding application) may not have it, so a
        failure to install them is tolerated.
        """

        def _handler(signum: int, _frame: Any) -> None:
            logger.info("received signal %s, shutting down", signum)
            self._stop.set()

        try:
            for sig in (signal.SIGINT, signal.SIGTERM):
                signal.signal(sig, _handler)
        except (ValueError, AttributeError):  # pragma: no cover - non-main thread
            logger.debug("signal handlers could not be installed on this thread")

    # -- handlers ----------------------------------------------------------

    def handle_job_message(self, payload: bytes) -> None:
        """Handle one job. Raising requests redelivery."""
        started = time.monotonic()

        try:
            raw = decode(payload)
        except BusError as exc:
            # Malformed bytes will never parse, so redelivering them would burn the
            # delivery budget for nothing. The failure is counted and swallowed.
            self.stats.decode_failures += 1
            logger.error("discarding a malformed job payload: %s", exc)
            return

        try:
            job = parse_job(raw)
        except UnknownJobKindError as exc:
            self.stats.decode_failures += 1
            logger.error("discarding a job with an unsupported kind: %s", exc)
            return

        result = dispatch(
            job, tenant_id=self.config.default_tenant_id, max_tokens=self.config.analysis_max_tokens
        )
        duration = time.monotonic() - started

        self.stats.jobs_handled += 1
        if result.status == STATUS_FAILED:
            self.stats.jobs_failed += 1
        if job.kind == KIND_PROMPT_ANALYSIS and result.status == STATUS_OK:
            analysis = result.detail.get("analysis")
            if isinstance(analysis, Mapping):
                record_analysis(self.metrics, analysis.get("flags") or [])

        record_job(self.metrics, job.kind, result.status, duration)

        payload_out = result.to_dict()
        payload_out["engine_version"] = ENGINE_VERSION
        subject = SUBJECT_PROMPT_ANALYSIS if job.kind == KIND_PROMPT_ANALYSIS else SUBJECT_EVAL_RESULT
        self.bus.publish_json(subject, payload_out)
        self.stats.results_published += 1

        logger.info(
            "handled job",
            extra={
                "job_id": job.id,
                "kind": job.kind,
                "status": result.status,
                "duration_seconds": round(duration, 6),
                "tenant_id": result.tenant_id,
            },
        )

    def handle_usage_message(self, payload: bytes) -> None:
        """Handle one usage event. Raising requests redelivery."""
        try:
            raw = decode(payload)
        except BusError as exc:
            self.stats.decode_failures += 1
            logger.error("discarding a malformed usage payload: %s", exc)
            return

        event = UsageEvent.from_dict(raw)
        self.aggregator.observe(event)
        self.stats.usage_events += 1
        record_usage(self.metrics, event)

    # -- rollups -----------------------------------------------------------

    def publish_rollup(self) -> Mapping[str, Any]:
        """Publish the current aggregate, returning the payload."""
        payload = dict(rollup_payload(self.aggregator))
        payload["engine_version"] = ENGINE_VERSION
        payload["worker_version"] = __version__
        self.bus.publish_json(SUBJECT_TELEMETRY_ROLLUP, payload)
        self.stats.rollups_published += 1
        self.metrics.increment(METRIC_ROLLUP_PUBLISHED_TOTAL)

        for provider in payload.get("providers", []):
            error_rate = provider.get("error_rate", 0.0)
            self.metrics.set(
                "synapass_worker_provider_error_rate",
                float(error_rate),
                provider=str(provider.get("provider", "unknown")),
            )
        return payload

    def _rollup_loop(self) -> None:
        """Publish a rollup every 30 seconds until stopped.

        A fixed interval rather than a configurable one: the rollup is consumed by
        dashboards and the router's latency priors, both of which expect a steady
        cadence, and a tunable interval would produce graphs whose smoothness
        depends on configuration.
        """
        interval = 30.0
        while not self._stop.wait(timeout=interval):
            try:
                self.aggregator.expire()
                self.publish_rollup()
            except Exception as exc:
                logger.error("failed to publish a telemetry rollup: %s", exc)

    # -- reporting ---------------------------------------------------------

    def health(self) -> dict[str, Any]:
        """Render the health snapshot served on the metrics port."""
        bus_stats = self.bus.stats()
        if bus_stats.get("reconnects"):
            self.metrics.set(METRIC_BUS_RECONNECTS_TOTAL, float(bus_stats["reconnects"]))
        return {
            "status": "ok" if bus_stats.get("connected") else "degraded",
            "version": __version__,
            "engine_version": ENGINE_VERSION,
            "started_at": datetime.fromtimestamp(self.stats.started_at or time.time(), tz=UTC).isoformat(),
            "stats": self.stats.as_dict(),
            "bus": bus_stats,
            "config": self.config.to_dict(redact=True),
            "totals": self.aggregator.totals(),
        }
