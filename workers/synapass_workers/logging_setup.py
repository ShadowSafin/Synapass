"""Structured logging for the workers.

The Go control plane emits JSON at info level with a stable set of keys. A worker
that emitted free-form text would be the one log source a Loki query cannot
correlate, so the same shape is used here: ``time``, ``level``, ``msg`` plus
whatever fields the call site attaches.

Redaction is applied to every record. The workers handle captured prompts, so a
naive log line is the most likely way personal data escapes the system.
"""

from __future__ import annotations

import json
import logging
import re
import sys
from collections.abc import Iterable, Mapping
from datetime import UTC, datetime
from typing import Any

# Keys whose values are replaced before a record is written. Matching is
# case-insensitive and substring-based, so ``api_key``, ``provider_api_key`` and
# ``X-Api-Key`` are all caught by one entry.
SENSITIVE_KEY_PARTS: tuple[str, ...] = (
    "authorization",
    "api_key",
    "apikey",
    "token",
    "secret",
    "password",
    "credential",
    "prompt",
    "completion",
    "content",
)

# Even a redacted prompt is a risk, so values are truncated as well as key-redacted.
MAX_VALUE_CHARS = 512

_REDACTED = "<redacted>"

# The standard LogRecord attributes. Anything outside this set is a field the
# caller attached with ``extra=``, which is what gets promoted into the JSON body.
_RESERVED: frozenset[str] = frozenset(
    [
        "args",
        "asctime",
        "created",
        "exc_info",
        "exc_text",
        "filename",
        "funcName",
        "levelname",
        "levelno",
        "lineno",
        "module",
        "msecs",
        "message",
        "msg",
        "name",
        "pathname",
        "process",
        "processName",
        "relativeCreated",
        "stack_info",
        "taskName",
        "thread",
        "threadName",
    ]
)

_SAFE_KEY = re.compile(r"[^A-Za-z0-9_]")


class JsonFormatter(logging.Formatter):
    """Render a log record as a single JSON object."""

    def __init__(self, service: str = "synapass-workers", labels: Mapping[str, str] | None = None) -> None:
        super().__init__()
        self.service = service
        self.labels = dict(labels or {})

    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, Any] = {
            "time": datetime.fromtimestamp(record.created, tz=UTC).isoformat(),
            "level": record.levelname.lower(),
            "msg": record.getMessage(),
            "service": self.service,
            "logger": record.name,
        }
        payload.update(self.labels)

        if record.exc_info:
            # The traceback is kept verbatim: it is the one place a stack is
            # genuinely needed, and it contains no user data by construction.
            payload["error"] = self.formatException(record.exc_info)

        for key, value in record.__dict__.items():
            if key in _RESERVED or key.startswith("_"):
                continue
            payload[_SAFE_KEY.sub("_", key)] = _safe_value(key, value)

        return json.dumps(payload, default=str, ensure_ascii=False)


class ConsoleFormatter(logging.Formatter):
    """Human-readable formatter for interactive use."""

    def __init__(self) -> None:
        super().__init__("%(asctime)s %(levelname)-7s %(name)s %(message)s")

    def format(self, record: logging.LogRecord) -> str:
        base = super().format(record)
        extras = {
            key: _safe_value(key, value)
            for key, value in record.__dict__.items()
            if key not in _RESERVED and not key.startswith("_")
        }
        if extras:
            rendered = " ".join(f"{key}={value}" for key, value in sorted(extras.items()))
            return f"{base} {rendered}"
        return base


def _safe_value(key: str, value: Any) -> Any:
    """Redact or bound a single field value."""
    lowered = key.lower()
    if any(part in lowered for part in SENSITIVE_KEY_PARTS):
        return _REDACTED
    if isinstance(value, str):
        if len(value) > MAX_VALUE_CHARS:
            return value[:MAX_VALUE_CHARS] + f"...<truncated {len(value) - MAX_VALUE_CHARS} chars>"
        return value
    if isinstance(value, Mapping):
        return {str(k): _safe_value(str(k), v) for k, v in value.items()}
    if isinstance(value, (list, tuple, set)):
        return [_safe_value(key, item) for item in value]
    return value


def configure(
    level: str = "info",
    fmt: str = "json",
    *,
    service: str = "synapass-workers",
    labels: Mapping[str, str] | None = None,
) -> None:
    """Install the process-wide logging configuration."""
    handler = logging.StreamHandler(sys.stdout)
    if fmt == "console":
        handler.setFormatter(ConsoleFormatter())
    else:
        handler.setFormatter(JsonFormatter(service=service, labels=labels))

    root = logging.getLogger()
    # Replacing handlers rather than adding one keeps a second call to configure
    # from doubling every log line, which happens in tests that build a worker more
    # than once.
    root.handlers = [handler]
    root.setLevel(_level_value(level))

    # Third-party libraries are noisy at debug level and their messages are not
    # structured the way ours are, so they are pinned to warning.
    for noisy in ("nats", "httpx", "httpcore", "urllib3"):
        logging.getLogger(noisy).setLevel(logging.WARNING)


def _level_value(level: str) -> int:
    normalised = (level or "info").lower()
    if normalised == "warn":
        normalised = "warning"
    return getattr(logging, normalised.upper(), logging.INFO)


def redact_value(key: str, value: Any) -> Any:
    """Expose redaction for callers that build a payload before logging it."""
    return _safe_value(key, value)


def redact_mapping(mapping: Mapping[str, Any]) -> dict[str, Any]:
    """Redact every value in a mapping."""
    return {str(key): _safe_value(str(key), value) for key, value in mapping.items()}


def redact_keys(mapping: Mapping[str, Any], keys: Iterable[str]) -> dict[str, Any]:
    """Redact a specific set of keys, leaving everything else untouched."""
    drop = {key.lower() for key in keys}
    return {key: (_REDACTED if key.lower() in drop else value) for key, value in mapping.items()}
