"""Message bus integration.

``nats-py`` is imported inside :meth:`Bus.connect` rather than at module import
time. That keeps the pure half of this package importable on a bare interpreter,
which is what makes the test suite runnable without installing anything.

# Handler contract

A subscription handler receives the raw payload and returns ``None``. Raising any
exception is the way to ask for redelivery: the bus translates that into a
negative acknowledgement. This is deliberately the only signalling mechanism,
because a boolean return invites a caller to return ``False`` by accident and
silently drop a job.
"""

from __future__ import annotations

import contextlib
import json
import logging
from collections.abc import Callable, Mapping
from typing import Any

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Subjects -- these strings must match internal/storage/nats.go exactly.
# ---------------------------------------------------------------------------

SUBJECT_USAGE_RECORDED = "ar.usage.recorded"
SUBJECT_TRACE_RECORDED = "ar.trace.recorded"
SUBJECT_REQUEST_LOGGED = "ar.log.request"
SUBJECT_PROVIDER_HEALTH = "ar.provider.health"
SUBJECT_PROVIDER_STATUS_CHANGE = "ar.provider.status"
SUBJECT_EVAL_JOB = "ar.eval.job"
SUBJECT_EVAL_RESULT = "ar.eval.result"
SUBJECT_REPLAY_JOB = "ar.replay.job"
SUBJECT_AUDIT_EVENT = "ar.audit.event"

# Subjects the workers own. They are published by the workers, not the control
# plane, and are kept in the same namespace so one stream can cover both.
SUBJECT_TELEMETRY_ROLLUP = "ar.telemetry.rollup"
SUBJECT_PROMPT_ANALYSIS = "ar.prompt.analysis"

# Durable consumer names. They are stable across restarts by design: a worker that
# picked a random durable name would create a new consumer every deploy and
# reprocess the whole stream.
DURABLE_TELEMETRY = "synapass-telemetry-worker"
DURABLE_INTELLIGENCE = "synapass-intelligence-worker"

PayloadHandler = Callable[[bytes], None]
DecodedHandler = Callable[[Mapping[str, Any]], None]


class BusError(RuntimeError):
    """Raised when the bus cannot be used."""


class Bus:
    """A wrapper over a NATS connection with JetStream support.

    nats-py 2.x is asyncio-only, but the worker runtime is threaded and sync.
    This class owns a dedicated event loop in a background thread and exposes
    a sync API over it, so callers never touch asyncio directly.
    """

    def __init__(
        self,
        url: str,
        *,
        name: str = "synapass-workers",
        credentials_file: str = "",
        token: str = "",
        jetstream: bool = True,
        max_deliver: int = 5,
        ack_wait_seconds: int = 120,
    ) -> None:
        self.url = url
        self.name = name
        self.credentials_file = credentials_file
        self.token = token
        self.jetstream = jetstream
        self.max_deliver = max_deliver
        self.ack_wait_seconds = ack_wait_seconds

        self._conn: Any = None
        self._js: Any = None
        self._subscriptions: list[Any] = []
        self._published = 0
        self._failed = 0

        self._loop: Any = None
        self._loop_thread: Any = None

    # -- lifecycle ---------------------------------------------------------

    def _ensure_loop(self) -> Any:
        """Start the background event loop on first use."""
        import asyncio
        import threading

        if self._loop is not None:
            return self._loop
        loop: Any = asyncio.new_event_loop()
        self._loop = loop

        def _run() -> None:
            asyncio.set_event_loop(loop)
            loop.run_forever()

        thread = threading.Thread(target=_run, name="nats-loop", daemon=True)
        thread.start()
        self._loop_thread = thread
        return loop

    def _call(self, coro: Any, timeout: float = 15.0) -> Any:
        """Run a coroutine on the background loop and wait for it."""
        import asyncio

        loop = self._ensure_loop()
        future = asyncio.run_coroutine_threadsafe(coro, loop)
        return future.result(timeout=timeout)

    def connect(self) -> None:
        """Connect to NATS, provisioning the streams the workers need.

        Reconnection is unbounded on purpose: a worker that gives up after a few
        attempts turns a brief broker restart into a permanently idle pool, and
        every transition is logged so a reconnect loop is visible rather than
        looking like a healthy idle process.
        """
        import asyncio

        async def _connect() -> None:
            import nats

            options: dict[str, Any] = {
                "name": self.name,
                "max_reconnect_attempts": -1,
                "reconnect_time_wait": 2,
                "allow_reconnect": True,
            }
            if self.token:
                options["token"] = self.token
            if self.credentials_file:
                options["user_credentials"] = self.credentials_file

            async def _do_connect() -> Any:
                return await asyncio.wait_for(nats.connect(self.url, **options), timeout=15)

            try:
                self._conn = await _do_connect()
            except Exception as exc:
                raise BusError(f"could not connect to nats at {self.url}: {exc}") from exc

            if self.jetstream:
                self._js = self._conn.jetstream()
                await self._ensure_streams_async()

            logger.info("connected to nats at %s", self.url)

        self._call(_connect(), timeout=30)

    async def _ensure_streams_async(self) -> None:
        """Create the streams a worker may consume from.

        The Go control plane provisions these too. Creating them here as well makes
        a worker pool independently startable, which matters when the intelligence
        tier is deployed separately from the gateway.
        """
        if self._js is None:
            return

        from .streams import STREAM_DEFINITIONS

        for definition in STREAM_DEFINITIONS:
            try:
                await self._js.stream_info(definition["name"])
                continue
            except Exception:
                pass
            try:
                await self._js.add_stream(**definition)
                logger.info("created jetstream stream %s", definition["name"])
            except Exception as exc:
                # Reported, not fatal: on a shared NATS cluster the worker may lack
                # permission to provision streams while still being allowed to
                # consume existing ones.
                logger.warning("could not create stream %s: %s", definition["name"], exc)

    def ensure_streams(self) -> None:
        """Create the streams a worker may consume from (sync wrapper)."""
        if self._js is None or self._loop is None:
            return
        self._call(self._ensure_streams_async(), timeout=30)

    def close(self) -> None:
        """Drain subscriptions and close the connection."""
        if self._loop is not None:
            with contextlib.suppress(Exception):
                self._call(self._close_async(), timeout=10)
            with contextlib.suppress(Exception):
                self._loop.call_soon_threadsafe(self._loop.stop)
            self._loop = None
            self._loop_thread = None
        self._subscriptions.clear()
        self._conn = None
        self._js = None

    async def _close_async(self) -> None:
        for subscription in self._subscriptions:
            with contextlib.suppress(Exception):
                await subscription.unsubscribe()
        self._subscriptions.clear()
        if self._conn is not None:
            try:
                await self._conn.drain()
            except Exception:
                with contextlib.suppress(Exception):
                    await self._conn.close()
            self._conn = None
            self._js = None

    # -- publishing --------------------------------------------------------

    def publish_json(self, subject: str, payload: Mapping[str, Any]) -> None:
        """Publish a JSON payload, counting failures rather than raising.

        A worker that failed a job because a *result* could not be published would
        retry work it has already done, so publishing is best-effort and observable
        through the failure counter.
        """
        if self._conn is None or self._loop is None:
            raise BusError("the bus is not connected")

        async def _publish() -> None:
            data = json.dumps(payload, default=str).encode("utf-8")
            await self._conn.publish(subject, data)
            with contextlib.suppress(Exception):
                await self._conn.flush(timeout=5)

        try:
            self._call(_publish(), timeout=10)
            self._published += 1
            # Flushing every publish bounds the window in which a crash loses the
            # result. The volume here is jobs, not requests, so the cost is nil.
        except Exception as exc:
            self._failed += 1
            logger.error("failed to publish to %s: %s", subject, exc)

    # -- subscribing -------------------------------------------------------

    def subscribe_jetstream(
        self,
        subject: str,
        durable: str,
        handler: PayloadHandler,
    ) -> Any:
        """Register a durable JetStream consumer.

        The handler runs on the delivery thread; raising requests redelivery.
        """
        if self._js is None or self._loop is None:
            raise BusError("jetstream is not enabled on this connection")

        async def _subscribe() -> Any:
            async def _wrapped(msg: Any) -> None:
                try:
                    handler(msg.data)
                except Exception as exc:
                    logger.warning(
                        "job failed on %s (durable %s), requesting redelivery: %s",
                        subject,
                        durable,
                        exc,
                    )
                    with contextlib.suppress(Exception):
                        await msg.nak()
                    return
                with contextlib.suppress(Exception):
                    await msg.ack()

            subscription = await self._js.subscribe(
                subject,
                durable=durable,
                manual_ack=True,
                cb=_wrapped,
            )
            return subscription

        subscription = self._call(_subscribe(), timeout=15)
        self._subscriptions.append(subscription)
        logger.info("subscribed to %s (durable %s)", subject, durable)
        return subscription

    def subscribe(self, subject: str, queue: str, handler: PayloadHandler) -> Any:
        """Register a core NATS queue subscription."""
        if self._conn is None or self._loop is None:
            raise BusError("the bus is not connected")

        async def _subscribe() -> Any:
            async def _wrapped(msg: Any) -> None:
                try:
                    handler(msg.data)
                except Exception as exc:
                    # Core NATS has no redelivery, so a failure is simply logged. This
                    # path is used for advisory traffic only.
                    logger.warning("handler failed on %s: %s", subject, exc)

            subscription = await self._conn.subscribe(subject, queue=queue, cb=_wrapped)
            return subscription

        subscription = self._call(_subscribe(), timeout=15)
        self._subscriptions.append(subscription)
        logger.info("subscribed to %s (queue %s)", subject, queue)
        return subscription

    # -- reporting ---------------------------------------------------------

    @property
    def connected(self) -> bool:
        if self._conn is None or self._loop is None:
            return False
        try:
            return bool(self._conn.is_connected)
        except Exception:
            return False

    def stats(self) -> dict[str, Any]:
        """Report connection health for the health endpoint."""
        if self._conn is None:
            return {"connected": False, "published": self._published, "failed": self._failed}
        stats = self._conn.stats()
        return {
            "connected": bool(self._conn.is_connected),
            "server": getattr(getattr(self._conn, "connected_url", None), "netloc", ""),
            "published": self._published,
            "failed": self._failed,
            "in_msgs": getattr(stats, "in_msgs", 0),
            "out_msgs": getattr(stats, "out_msgs", 0),
            "reconnects": getattr(stats, "reconnects", 0),
        }


class NullBus:
    """A bus that records published payloads and delivers nothing.

    Used by the tests and by the one-shot CLI commands, which analyse or score a
    single input without needing a message broker.
    """

    def __init__(self) -> None:
        self.published: list[tuple[str, Mapping[str, Any]]] = []

    def connect(self) -> None:
        return None

    def close(self) -> None:
        return None

    def publish_json(self, subject: str, payload: Mapping[str, Any]) -> None:
        self.published.append((subject, payload))

    @property
    def connected(self) -> bool:
        return True

    def stats(self) -> dict[str, Any]:
        return {"connected": True, "published": len(self.published), "failed": 0, "null": True}

    def messages_for(self, subject: str) -> list[Mapping[str, Any]]:
        """Return every payload published on a subject."""
        return [payload for published_subject, payload in self.published if published_subject == subject]


def decode(payload: bytes) -> Mapping[str, Any]:
    """Decode a JSON payload, raising :class:`BusError` on malformed input.

    Malformed input is treated as a permanent failure by the caller rather than a
    transient one: redelivering bytes that will never parse just fills the
    redelivery budget.
    """
    try:
        decoded = json.loads(payload.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise BusError(f"payload is not valid JSON: {exc}") from exc
    if not isinstance(decoded, dict):
        raise BusError("payload must be a JSON object")
    return decoded
