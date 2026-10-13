"""Payload models shared with the Go control plane.

These are hand-written dataclasses rather than generated bindings. The set of
messages the workers exchange is small and changes deliberately, the control
plane may be a version ahead of the workers during a rollout, and what matters is
that an unrecognised field is *ignored* rather than fatal.

That tolerance is the design rule here: every ``from_dict`` reads the keys it
knows and skips the rest, so adding a field to a Go struct never makes an
older worker reject a message.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping, Sequence
from dataclasses import dataclass, field
from datetime import UTC, datetime
from typing import Any

# ---------------------------------------------------------------------------
# Message kinds
# ---------------------------------------------------------------------------

# Job kinds carried on the ar.eval.job subject. A single job subject with a kind
# discriminator is used rather than one subject per kind because the workers share
# a durable consumer and a queue group: routing by kind inside the handler means a
# new job type does not require a new consumer, a new durable name and a new
# deployment step.
KIND_EVAL = "eval"
KIND_PROMPT_ANALYSIS = "prompt_analysis"
KIND_REPLAY = "replay"

KNOWN_JOB_KINDS = frozenset({KIND_EVAL, KIND_PROMPT_ANALYSIS, KIND_REPLAY})

# Outcomes reported back for a job.
STATUS_OK = "ok"
STATUS_FAILED = "failed"
STATUS_SKIPPED = "skipped"


def utcnow() -> datetime:
    """Return the current UTC time."""
    return datetime.now(UTC)


def parse_timestamp(value: Any) -> datetime:
    """Parse an RFC 3339 timestamp, tolerating a trailing ``Z``.

    Python only learned to parse ``Z`` in 3.11, and the Go side emits RFC 3339 with
    a numeric offset, so both forms are accepted rather than relying on one.
    """
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=UTC)
    if not isinstance(value, str) or value == "":
        return utcnow()
    text = value.strip()
    if text.endswith("Z"):
        text = text[:-1] + "+00:00"
    try:
        parsed = datetime.fromisoformat(text)
    except ValueError:
        return utcnow()
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)


def _as_int(value: Any, default: int = 0) -> int:
    if isinstance(value, bool):
        return default
    if isinstance(value, int):
        return value
    if isinstance(value, float):
        return int(value)
    if isinstance(value, str):
        try:
            return int(float(value.strip()))
        except ValueError:
            return default
    return default


def _as_float(value: Any, default: float = 0.0) -> float:
    if isinstance(value, bool):
        return default
    if isinstance(value, (int, float)):
        return float(value)
    if isinstance(value, str):
        try:
            return float(value.strip())
        except ValueError:
            return default
    return default


def _as_str(value: Any, default: str = "") -> str:
    if isinstance(value, str):
        return value
    if value is None:
        return default
    return str(value)


def _as_bool(value: Any, default: bool = False) -> bool:
    if isinstance(value, bool):
        return value
    if isinstance(value, str):
        lowered = value.strip().lower()
        if lowered in {"1", "true", "yes", "on"}:
            return True
        if lowered in {"0", "false", "no", "off"}:
            return False
    if isinstance(value, (int, float)):
        return value != 0
    return default


# ---------------------------------------------------------------------------
# Telemetry events (consumed from the control plane)
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class TokenUsage:
    """Token accounting for one request."""

    prompt_tokens: int = 0
    completion_tokens: int = 0
    total_tokens: int = 0
    cached_prompt_tokens: int = 0
    estimated: bool = False

    @property
    def effective_total(self) -> int:
        """The total, derived from the halves when the provider omitted it."""
        if self.total_tokens:
            return self.total_tokens
        return self.prompt_tokens + self.completion_tokens

    @classmethod
    def from_dict(cls, raw: Mapping[str, Any] | None) -> TokenUsage:
        if not raw:
            return cls()
        usage = cls(
            prompt_tokens=_as_int(raw.get("prompt_tokens")),
            completion_tokens=_as_int(raw.get("completion_tokens")),
            total_tokens=_as_int(raw.get("total_tokens")),
            cached_prompt_tokens=_as_int(raw.get("cached_prompt_tokens")),
            estimated=_as_bool(raw.get("estimated")),
        )
        return usage


@dataclass(frozen=True)
class UsageEvent:
    """A completed request's usage record, mirroring the Go ``UsageRecord``.

    The field set is a subset of the Go struct: the workers only consume what they
    aggregate. Unknown keys on the wire are ignored, which is what keeps a
    control-plane release from breaking a running worker pool.
    """

    id: str = ""
    request_id: str = ""
    trace_id: str = ""
    tenant_id: str = ""
    api_key_id: str = ""
    provider: str = ""
    model: str = ""
    requested_model: str = ""
    policy_id: str = ""
    request_type: str = ""
    usage: TokenUsage = field(default_factory=TokenUsage)
    cost_usd: float = 0.0
    latency_ms: int = 0
    provider_latency_ms: int = 0
    attempts: int = 0
    fallback_used: bool = False
    cache_hit: bool = False
    outcome: str = ""
    error_code: str = ""
    streaming: bool = False
    status: int = 0
    created_at: datetime = field(default_factory=utcnow)

    @property
    def succeeded(self) -> bool:
        """Outcomes that count as served: a fallback is still a success."""
        return self.outcome in {"success", "fallback"}

    @classmethod
    def from_dict(cls, raw: Mapping[str, Any]) -> UsageEvent:
        return cls(
            id=_as_str(raw.get("id")),
            request_id=_as_str(raw.get("request_id")),
            trace_id=_as_str(raw.get("trace_id")),
            tenant_id=_as_str(raw.get("tenant_id")),
            api_key_id=_as_str(raw.get("api_key_id")),
            provider=_as_str(raw.get("provider")),
            model=_as_str(raw.get("model")),
            requested_model=_as_str(raw.get("requested_model")),
            policy_id=_as_str(raw.get("policy_id")),
            request_type=_as_str(raw.get("request_type")),
            usage=TokenUsage.from_dict(raw.get("usage")),
            cost_usd=_as_float(_nested(raw.get("cost"), "usd")),
            latency_ms=_as_int(raw.get("latency_ms")),
            provider_latency_ms=_as_int(raw.get("provider_latency_ms")),
            attempts=_as_int(raw.get("attempts")),
            fallback_used=_as_bool(raw.get("fallback_used")),
            cache_hit=_as_bool(raw.get("cache_hit")),
            outcome=_as_str(raw.get("outcome")),
            error_code=_as_str(raw.get("error_code")),
            streaming=_as_bool(raw.get("streaming")),
            status=_as_int(raw.get("status")),
            created_at=parse_timestamp(raw.get("created_at")),
        )


def _nested(raw: Any, key: str) -> Any:
    """Read ``raw[key]`` when raw is a mapping, else None.

    The Go ``Cost`` type serialises as an object, so a scalar cost on the wire is
    also accepted for forward compatibility with a flatter encoding.
    """
    if isinstance(raw, Mapping):
        return raw.get(key)
    if isinstance(raw, (int, float)) and key == "usd":
        return raw
    return None


# ---------------------------------------------------------------------------
# Evaluation jobs
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class EvalCandidate:
    """One model's answer to be scored."""

    id: str = ""
    provider: str = ""
    model: str = ""
    output: str = ""
    latency_ms: int = 0
    cost_usd: float = 0.0
    error: str = ""
    # judge_score is populated only when a judge model was used, and is recorded
    # separately from the deterministic scores so the two never blur together.
    judge_score: float | None = None

    @classmethod
    def from_dict(cls, raw: Mapping[str, Any]) -> EvalCandidate:
        judge = raw.get("judge_score")
        return cls(
            id=_as_str(raw.get("id")) or _as_str(raw.get("model")),
            provider=_as_str(raw.get("provider")),
            model=_as_str(raw.get("model")),
            output=_as_str(raw.get("output")),
            latency_ms=_as_int(raw.get("latency_ms")),
            cost_usd=_as_float(raw.get("cost_usd")),
            error=_as_str(raw.get("error")),
            judge_score=None if judge is None else _as_float(judge),
        )


@dataclass(frozen=True)
class EvalJob:
    """An evaluation job: score every candidate against a reference answer."""

    id: str = ""
    kind: str = KIND_EVAL
    tenant_id: str = ""
    request_id: str = ""
    dataset: str = ""
    reference: str = ""
    metrics: tuple[str, ...] = ()
    weights: Mapping[str, float] = field(default_factory=dict)
    candidates: tuple[EvalCandidate, ...] = ()
    # max_candidates bounds the work one job can create, so a malformed or hostile
    # job cannot make a worker allocate unbounded memory.
    max_candidates: int = 64
    created_at: datetime = field(default_factory=utcnow)
    payload: Mapping[str, Any] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, raw: Mapping[str, Any]) -> EvalJob:
        raw_candidates = raw.get("candidates")
        candidates: tuple[EvalCandidate, ...] = ()
        if isinstance(raw_candidates, Sequence) and not isinstance(raw_candidates, (str, bytes)):
            candidates = tuple(
                EvalCandidate.from_dict(item) for item in raw_candidates if isinstance(item, Mapping)
            )

        raw_metrics = raw.get("metrics")
        metrics: tuple[str, ...] = ()
        if isinstance(raw_metrics, Sequence) and not isinstance(raw_metrics, (str, bytes)):
            metrics = tuple(_as_str(item) for item in raw_metrics if _as_str(item))

        raw_weights = raw.get("weights")
        weights: dict[str, float] = {}
        if isinstance(raw_weights, Mapping):
            weights = {str(k): _as_float(v) for k, v in raw_weights.items()}

        return cls(
            id=_as_str(raw.get("id")) or _as_str(raw.get("job_id")),
            kind=_as_str(raw.get("kind")) or KIND_EVAL,
            tenant_id=_as_str(raw.get("tenant_id")),
            request_id=_as_str(raw.get("request_id")),
            dataset=_as_str(raw.get("dataset")),
            reference=_as_str(raw.get("reference")),
            metrics=metrics,
            weights=weights,
            candidates=candidates,
            max_candidates=_as_int(raw.get("max_candidates"), 64) or 64,
            created_at=parse_timestamp(raw.get("created_at")),
            payload=dict(raw),
        )


@dataclass(frozen=True)
class EvalResult:
    """The scored outcome of an evaluation job."""

    job_id: str
    tenant_id: str
    status: str
    engine_version: str
    ranking: tuple[tuple[str, float], ...] = ()
    scores: Mapping[str, Mapping[str, float]] = field(default_factory=dict)
    winner: str = ""
    candidate_count: int = 0
    skipped: tuple[str, ...] = ()
    notes: tuple[str, ...] = ()
    created_at: datetime = field(default_factory=utcnow)

    def to_dict(self) -> dict[str, Any]:
        """Render the result for publication."""
        return {
            "job_id": self.job_id,
            "tenant_id": self.tenant_id,
            "status": self.status,
            "engine_version": self.engine_version,
            "ranking": [{"candidate_id": cid, "score": score} for cid, score in self.ranking],
            "scores": {cid: dict(scores) for cid, scores in self.scores.items()},
            "winner": self.winner,
            "candidate_count": self.candidate_count,
            "skipped": list(self.skipped),
            "notes": list(self.notes),
            "created_at": self.created_at.astimezone(UTC).isoformat(),
        }


# ---------------------------------------------------------------------------
# Prompt analysis
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class PromptAnalysis:
    """Static analysis of a prompt, used to inform routing and to flag risk."""

    request_id: str = ""
    prompt_tokens: int = 0
    completion_tokens_estimate: int = 0
    message_count: int = 0
    capabilities: tuple[str, ...] = ()
    complexity: str = "simple"
    suggested_quality_tier: int = 2
    suggested_strategy: str = "priority"
    contains_code: bool = False
    wants_json: bool = False
    non_ascii_ratio: float = 0.0
    sensitive_patterns: tuple[str, ...] = ()
    flags: tuple[str, ...] = ()
    engine_version: str = ""

    def to_dict(self) -> dict[str, Any]:
        return {
            "request_id": self.request_id,
            "prompt_tokens": self.prompt_tokens,
            "completion_tokens_estimate": self.completion_tokens_estimate,
            "message_count": self.message_count,
            "capabilities": list(self.capabilities),
            "complexity": self.complexity,
            "suggested_quality_tier": self.suggested_quality_tier,
            "suggested_strategy": self.suggested_strategy,
            "contains_code": self.contains_code,
            "wants_json": self.wants_json,
            "non_ascii_ratio": self.non_ascii_ratio,
            "sensitive_patterns": list(self.sensitive_patterns),
            "flags": list(self.flags),
            "engine_version": self.engine_version,
        }


@dataclass(frozen=True)
class JobResult:
    """A published result envelope, generic over the job kind."""

    job_id: str
    kind: str
    tenant_id: str
    status: str
    detail: Mapping[str, Any] = field(default_factory=dict)
    error: str = ""

    def to_dict(self) -> dict[str, Any]:
        payload = {
            "job_id": self.job_id,
            "kind": self.kind,
            "tenant_id": self.tenant_id,
            "status": self.status,
            "engine_version": "",
            "created_at": utcnow().astimezone(UTC).isoformat(),
        }
        payload.update(self.detail)
        if self.error:
            payload["error"] = self.error
        return payload


def iter_dicts(raw: Any) -> Iterable[Mapping[str, Any]]:
    """Yield mapping items from a list, skipping anything else."""
    if isinstance(raw, Sequence) and not isinstance(raw, (str, bytes)):
        for item in raw:
            if isinstance(item, Mapping):
                yield item
