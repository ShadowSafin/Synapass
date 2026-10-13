"""Request replay: re-execute captured traffic offline for comparison.

Replay is deterministic and safe: it never calls a live provider here. Instead
it validates the job, selects the requests to replay, and renders the
execution plan the Go control plane (or an operator) will run. Scoring of the
replayed outputs reuses the deterministic scorers, so a replay result is
reproducible byte-for-byte.
"""

from __future__ import annotations

from collections.abc import Sequence
from typing import Any

from .models import EvalCandidate, EvalJob


def validate_replay_job(job: EvalJob) -> list[str]:
    """Return a list of validation errors; empty means valid."""
    errors: list[str] = []
    if not job.request_id and not job.dataset and not job.payload.get("request_ids"):
        # EvalJob carries request_ids in payload for replay jobs.
        raw_ids = job.payload.get("request_ids")
        if not raw_ids:
            errors.append("replay job requires request_ids or dataset")
    raw_ids = job.payload.get("request_ids", [])
    if raw_ids and not isinstance(raw_ids, (list, tuple)):
        errors.append("request_ids must be a list")
    if isinstance(raw_ids, (list, tuple)) and len(raw_ids) > 1000:
        errors.append("request_ids exceeds the 1000-request limit")
    providers = job.payload.get("providers", [])
    if providers and not isinstance(providers, (list, tuple)):
        errors.append("providers must be a list")
    return errors


def select_requests(
    available: Sequence[str],
    requested: Sequence[str] | None,
    max_requests: int = 100,
) -> list[str]:
    """Select the request IDs to replay, preserving requested order."""
    if max_requests <= 0:
        max_requests = 100
    max_requests = min(max_requests, 1000)
    if not requested:
        return list(available[:max_requests])
    wanted = [r for r in requested if r in set(available)] if available else list(requested)
    # When no availability list is given, honor the request as-is (bounded).
    if not available:
        wanted = list(requested)
    return wanted[:max_requests]


def build_replay_plan(
    job: EvalJob,
    *,
    max_requests: int = 100,
) -> dict[str, Any]:
    """Render the execution plan for a replay job."""
    raw_ids = job.payload.get("request_ids", [])
    requested = [str(x) for x in raw_ids if str(x)] if isinstance(raw_ids, (list, tuple)) else []
    providers = job.payload.get("providers", [])
    models = job.payload.get("models", [])
    if isinstance(providers, str):
        providers = [providers]
    if isinstance(models, str):
        models = [models]
    selected = select_requests([], requested, max_requests or job.max_candidates or 100)
    return {
        "job_id": job.id,
        "tenant_id": job.tenant_id,
        "dataset": job.dataset,
        "request_ids": selected,
        "request_count": len(selected),
        "providers": list(providers) if isinstance(providers, (list, tuple)) else [],
        "models": list(models) if isinstance(models, (list, tuple)) else [],
        "metrics": list(job.metrics) if job.metrics else ["token_f1", "char_ngram", "exact_match"],
    }


def score_replayed(
    reference: str,
    outputs: Sequence[EvalCandidate],
    metrics: Sequence[str] | None = None,
) -> dict[str, Any]:
    """Score replayed outputs against a reference (thin wrapper over scoring)."""
    from .scoring import resolve_metrics, score_candidate

    names = resolve_metrics(metrics)
    scores: dict[str, dict[str, float]] = {}
    for out in outputs:
        if out.error:
            continue
        scores[out.id or out.model] = score_candidate(reference, out.output, names)
    return {"metrics": names, "scores": scores, "count": len(scores)}
