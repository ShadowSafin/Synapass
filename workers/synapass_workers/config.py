"""Worker configuration.

Every setting is reachable from the environment, so a container or a systemd unit
can be fully configured without a config file, and a config file can be used
without touching the environment. The precedence mirrors the Go control plane:
defaults, then an optional YAML/JSON file, then ``SYNAPASS_WORKER_*`` variables.

Nothing here reads a secret from a file that would be committed.
"""

from __future__ import annotations

import json
import os
from collections.abc import Mapping
from dataclasses import dataclass, field, replace
from pathlib import Path
from typing import Any

# Environment prefix. ``SYNAPASS_WORKER_`` is used rather than ``SYNAPASS_`` so a worker and
# the control plane can share an environment without one silently reconfiguring
# the other.
ENV_PREFIX = "SYNAPASS_WORKER_"

# Defaults. They are constants rather than inline literals so the shipped
# example configuration and the code cannot drift apart.
DEFAULT_NATS_URL = "nats://localhost:4222"
DEFAULT_QUEUE_GROUP = "synapass-workers"
DEFAULT_METRICS_ADDR = "0.0.0.0:9101"
DEFAULT_HTTP_TIMEOUT_SECONDS = 60.0
DEFAULT_EVAL_CONCURRENCY = 4
DEFAULT_ANALYSIS_MAX_TOKENS = 128_000


class ConfigError(ValueError):
    """Raised when the environment describes an unusable configuration.

    A worker that cannot reach its message bus is useless, so a bad value is
    reported at startup rather than degrading into silent inactivity.
    """


def _env(environ: Mapping[str, str], *names: str) -> str:
    """Return the first non-empty value among ``names``."""
    for name in names:
        raw = environ.get(name)
        if raw is not None and raw.strip() != "":
            return raw.strip()
    return ""


def _env_bool(environ: Mapping[str, str], *names: str, default: bool) -> bool:
    """Parse a boolean, accepting the spellings operators actually write."""
    raw = _env(environ, *names).lower()
    if raw == "":
        return default
    if raw in {"1", "true", "yes", "y", "on"}:
        return True
    if raw in {"0", "false", "no", "n", "off"}:
        return False
    raise ConfigError(f"{names[0]} must be a boolean, got {raw!r}")


def _env_int(environ: Mapping[str, str], *names: str, default: int, minimum: int = 1) -> int:
    raw = _env(environ, *names)
    if raw == "":
        return default
    try:
        value = int(raw)
    except ValueError as exc:
        raise ConfigError(f"{names[0]} must be an integer, got {raw!r}") from exc
    if value < minimum:
        raise ConfigError(f"{names[0]} must be at least {minimum}, got {value}")
    return value


def _env_float(environ: Mapping[str, str], *names: str, default: float, minimum: float = 0.0) -> float:
    raw = _env(environ, *names)
    if raw == "":
        return default
    try:
        value = float(raw)
    except ValueError as exc:
        raise ConfigError(f"{names[0]} must be a number, got {raw!r}") from exc
    if value < minimum:
        raise ConfigError(f"{names[0]} must be at least {minimum}, got {value}")
    return value


@dataclass(frozen=True)
class Config:
    """Resolved worker configuration."""

    # --- messaging ---
    nats_url: str = DEFAULT_NATS_URL
    nats_name: str = "synapass-workers"
    nats_credentials_file: str = ""
    nats_token: str = ""
    jetstream: bool = True
    queue_group: str = DEFAULT_QUEUE_GROUP
    ack_wait_seconds: int = 120
    max_deliver: int = 5

    # --- optional model provider, used as an offline judge ---
    # An empty URL disables judging entirely. Local runtimes are the intended
    # target: scoring a candidate set with a hosted model would make the eval
    # worker depend on the very providers it is meant to be measuring.
    provider_url: str = ""
    provider_api_key: str = ""
    provider_model: str = ""
    provider_timeout_seconds: float = DEFAULT_HTTP_TIMEOUT_SECONDS

    # --- analysis limits ---
    analysis_max_tokens: int = DEFAULT_ANALYSIS_MAX_TOKENS
    eval_concurrency: int = DEFAULT_EVAL_CONCURRENCY

    # --- observability ---
    metrics_enabled: bool = True
    metrics_addr: str = DEFAULT_METRICS_ADDR
    log_level: str = "info"
    log_format: str = "json"

    # --- attribution ---
    # default_tenant_id attributes results for jobs that arrive without a tenant.
    default_tenant_id: str = ""
    labels: Mapping[str, str] = field(default_factory=dict)

    def __post_init__(self) -> None:
        if self.provider_url and not self.provider_model:
            raise ConfigError(
                "provider_url is set but provider_model is empty; name the model the judge endpoint expects"
            )

    @property
    def judging_enabled(self) -> bool:
        """Whether an offline judge model is configured."""
        return bool(self.provider_url and self.provider_model)

    def to_dict(self, redact: bool = True) -> dict[str, Any]:
        """Render the configuration for logs and the health endpoint.

        Secrets are replaced with a presence marker rather than their value, so
        the same rendering can be logged and served without a second audit.
        """
        out: dict[str, Any] = {
            "nats_url": self.nats_url,
            "nats_name": self.nats_name,
            "jetstream": self.jetstream,
            "queue_group": self.queue_group,
            "ack_wait_seconds": self.ack_wait_seconds,
            "max_deliver": self.max_deliver,
            "provider_url": self.provider_url,
            "provider_model": self.provider_model,
            "judging_enabled": self.judging_enabled,
            "provider_timeout_seconds": self.provider_timeout_seconds,
            "analysis_max_tokens": self.analysis_max_tokens,
            "eval_concurrency": self.eval_concurrency,
            "metrics_enabled": self.metrics_enabled,
            "metrics_addr": self.metrics_addr,
            "log_level": self.log_level,
            "log_format": self.log_format,
            "default_tenant_id": self.default_tenant_id,
            "labels": dict(self.labels),
        }
        if redact:
            out["nats_token"] = "<set>" if self.nats_token else ""
            out["nats_credentials_file"] = "<set>" if self.nats_credentials_file else ""
            out["provider_api_key"] = "<set>" if self.provider_api_key else ""
        else:  # pragma: no cover - only reachable by an explicit opt-in
            out["nats_token"] = self.nats_token
            out["provider_api_key"] = self.provider_api_key
        return out

    @classmethod
    def from_env(cls, environ: Mapping[str, str] | None = None) -> Config:
        """Build a configuration from the environment."""
        env = os.environ if environ is None else environ

        config = cls(
            nats_url=_env(env, ENV_PREFIX + "NATS_URL", "NATS_URL") or DEFAULT_NATS_URL,
            nats_name=_env(env, ENV_PREFIX + "NATS_NAME") or "synapass-workers",
            nats_credentials_file=_env(env, ENV_PREFIX + "NATS_CREDENTIALS_FILE"),
            nats_token=_env(env, ENV_PREFIX + "NATS_TOKEN", "NATS_TOKEN"),
            jetstream=_env_bool(env, ENV_PREFIX + "NATS_JETSTREAM", default=True),
            queue_group=_env(env, ENV_PREFIX + "QUEUE_GROUP") or DEFAULT_QUEUE_GROUP,
            ack_wait_seconds=_env_int(env, ENV_PREFIX + "ACK_WAIT_SECONDS", default=120),
            max_deliver=_env_int(env, ENV_PREFIX + "MAX_DELIVER", default=5),
            provider_url=_env(env, ENV_PREFIX + "PROVIDER_URL"),
            provider_api_key=_env(env, ENV_PREFIX + "PROVIDER_API_KEY"),
            provider_model=_env(env, ENV_PREFIX + "PROVIDER_MODEL"),
            provider_timeout_seconds=_env_float(
                env,
                ENV_PREFIX + "PROVIDER_TIMEOUT_SECONDS",
                default=DEFAULT_HTTP_TIMEOUT_SECONDS,
                minimum=1.0,
            ),
            analysis_max_tokens=_env_int(
                env, ENV_PREFIX + "ANALYSIS_MAX_TOKENS", default=DEFAULT_ANALYSIS_MAX_TOKENS
            ),
            eval_concurrency=_env_int(env, ENV_PREFIX + "EVAL_CONCURRENCY", default=DEFAULT_EVAL_CONCURRENCY),
            metrics_enabled=_env_bool(env, ENV_PREFIX + "METRICS_ENABLED", default=True),
            metrics_addr=_env(env, ENV_PREFIX + "METRICS_ADDR") or DEFAULT_METRICS_ADDR,
            log_level=(_env(env, ENV_PREFIX + "LOG_LEVEL", "LOG_LEVEL") or "info").lower(),
            log_format=(_env(env, ENV_PREFIX + "LOG_FORMAT", "LOG_FORMAT") or "json").lower(),
            default_tenant_id=_env(env, ENV_PREFIX + "DEFAULT_TENANT_ID"),
            labels=_parse_labels(_env(env, ENV_PREFIX + "LABELS")),
        )

        if config.log_level not in {"debug", "info", "warning", "warn", "error"}:
            raise ConfigError(f"log_level must be debug, info, warning or error, got {config.log_level!r}")
        if config.log_format not in {"json", "console"}:
            raise ConfigError(f"log_format must be json or console, got {config.log_format!r}")
        if not config.nats_url:
            raise ConfigError("nats_url must not be empty")

        return config

    def with_overrides(self, **overrides: Any) -> Config:
        """Return a copy with the given fields replaced."""
        return replace(self, **overrides)

    @classmethod
    def from_file(cls, path: str | Path) -> dict[str, Any]:
        """Load a JSON or YAML configuration file into keyword arguments.

        YAML support is optional: it needs PyYAML, which the workers do not
        otherwise depend on. A JSON file is always readable, so a deployment that
        cannot install PyYAML still has a config-file path.
        """
        text = Path(path).read_text(encoding="utf-8")
        if str(path).endswith((".yaml", ".yml")):
            try:
                import yaml  # type: ignore[import-not-found]
            except ImportError as exc:  # pragma: no cover - depends on the host
                raise ConfigError(
                    "reading a YAML config file requires PyYAML; install it or use a JSON config file"
                ) from exc
            loaded = yaml.safe_load(text)
        else:
            loaded = json.loads(text)

        if loaded is None:
            return {}
        if not isinstance(loaded, dict):
            raise ConfigError("the config file must contain a mapping at the top level")
        return loaded


def _parse_labels(raw: str) -> Mapping[str, str]:
    """Parse ``k=v,k=v`` label syntax.

    Labels are propagated into logs and metrics, which is how an operator tells
    two worker pools apart in a dashboard that aggregates them.
    """
    if not raw:
        return {}
    labels: dict[str, str] = {}
    for pair in raw.split(","):
        key, sep, value = pair.partition("=")
        key = key.strip()
        if not sep or not key:
            raise ConfigError(f"labels must be written as key=value pairs, got {pair!r}")
        labels[key] = value.strip()
    return labels
