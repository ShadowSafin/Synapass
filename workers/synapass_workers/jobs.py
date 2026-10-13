"""Job handling.

The interesting decisions live in the two pure functions below --
:func:`evaluate_job` and :func:`analyze_job` -- so they can be tested without a
message bus, and :func:`dispatch` is a thin router over them. A worker that
couples its business logic to a JetStream subscription cannot be tested, and
these are the parts most likely to be wrong.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence
from typing import Any

from . import ENGINE_VERSION
from .models import (
    KIND_EVAL,
    KIND_PROMPT_ANALYSIS,
    KIND_REPLAY,
    KNOWN_JOB_KINDS,
    STATUS_FAILED,
    STATUS_OK,
    STATUS_SKIPPED,
    EvalJob,
    EvalResult,
    JobResult,
)
from .prompt_analysis import analyze_request
from .scoring import rank_candidates, resolve_metrics, score_candidate


class UnknownJobKindError(ValueError):
    """Raised when a job carries a ``kind`` this worker does not implement.

    Rejecting loudly is deliberate: silently acknowledging an unknown job would
    let a control-plane release enqueue work that is dropped without a trace,
    which is far harder to notice than a stream of failed jobs.
    """


def evaluate_job(
    job: EvalJob,
    *,
    tenant_id: str = "",
    include_failures: bool = False,
) -> EvalResult:
    """Score every candidate in a job against its reference answer.

    Candidates that carry an error are reported as skipped rather than scored.
    Scoring an error message against a reference would produce a number that looks
    like a quality measurement and is really a zero, which then enters an average
    and quietly drags a provider's reported quality down.

    A candidate set is bounded by ``job.max_candidates``; anything beyond that is
    skipped with a note, so a malformed job cannot be used to make a worker
    allocate unbounded memory.
    """
    metrics = resolve_metrics(job.metrics)
    notes: list[str] = []
    skipped: list[str] = []

    if job.max_candidates > 0 and len(job.candidates) > job.max_candidates:
        notes.append(
            f"job listed {len(job.candidates)} candidates; only the first {job.max_candidates} were scored"
        )
        candidates = job.candidates[: job.max_candidates]
    else:
        candidates = job.candidates

    scores: dict[str, dict[str, float]] = {}
    for candidate in candidates:
        if candidate.error and not include_failures:
            skipped.append(candidate.id or candidate.model)
            continue
        if not candidate.output and not candidate.error:
            # An empty answer is a real outcome (a filter, a truncated stream) and
            # scores zero rather than being dropped, because dropping it would
            # flatter the provider that produced it.
            notes.append(f"candidate {candidate.id or candidate.model} returned no output")
        scores[candidate.id or candidate.model] = score_candidate(job.reference, candidate.output, metrics)

    if not scores:
        return EvalResult(
            job_id=job.id,
            tenant_id=tenant_id or job.tenant_id,
            status=STATUS_SKIPPED,
            engine_version=ENGINE_VERSION,
            skipped=tuple(skipped),
            notes=(*notes, "no candidate produced a scoreable answer"),
            candidate_count=len(candidates),
        )

    weights = dict(job.weights) if job.weights else None
    ranking = tuple(rank_candidates(scores, weights))

    return EvalResult(
        job_id=job.id,
        tenant_id=tenant_id or job.tenant_id,
        status=STATUS_OK,
        engine_version=ENGINE_VERSION,
        ranking=ranking,
        scores=scores,
        winner=ranking[0][0] if ranking else "",
        candidate_count=len(candidates),
        skipped=tuple(skipped),
        notes=tuple(notes),
    )


def analyze_job(
    job: EvalJob,
    *,
    tenant_id: str = "",
    max_tokens: int = 0,
) -> dict[str, Any]:
    """Analyse the request embedded in a prompt-analysis job.

    The request is taken from the job's raw payload under ``request``; a job that
    carries only a ``prompt`` string is analysed as bare text. Both shapes are
    accepted because the control plane captures the former from live traffic and
    an operator writing a job by hand produces the latter.
    """
    request = job.payload.get("request")
    if not isinstance(request, Mapping):
        prompt = job.payload.get("prompt")
        if isinstance(prompt, str) and prompt:
            request = {"model": "", "messages": [{"role": "user", "content": prompt}]}
        else:
            return {
                "request_id": job.request_id or job.id,
                "error": "job carries neither a request object nor a prompt string",
                "engine_version": ENGINE_VERSION,
            }

    analysis = analyze_request(
        request,
        request_id=job.request_id or job.id,
        max_tokens=max_tokens,
    )
    analysis["tenant_id"] = tenant_id or job.tenant_id
    if job.dataset:
        analysis["dataset"] = job.dataset
    # Phase 2: task classification rides alongside prompt analysis so the
    # dashboard can show routing-relevant typing without a second job.
    try:
        from .task_classify import classify_task

        prompt_tokens = int(analysis.get("prompt_tokens", 0) or 0)
        task = classify_task(request, prompt_tokens)
        analysis["task"] = task.get("task")
        analysis["task_confidence"] = task.get("confidence")
        analysis["task_signals"] = task.get("signals")
        analysis["task_secondary"] = task.get("secondary")
    except Exception:
        pass
    return analysis


def dispatch(
    job: EvalJob,
    *,
    tenant_id: str = "",
    max_tokens: int = 0,
) -> JobResult:
    """Route a job to its handler and wrap the outcome in a result envelope.

    Every outcome -- success, skip or failure -- is returned as a result rather
    than raised. A job that failed silently would be redelivered until its
    delivery limit and then discarded, leaving no record of why an evaluation
    never ran; a published failure is at least visible in the results stream.
    """
    job_tenant = tenant_id or job.tenant_id

    try:
        if job.kind == KIND_EVAL:
            result = evaluate_job(job, tenant_id=job_tenant)
            return JobResult(
                job_id=job.id,
                kind=job.kind,
                tenant_id=job_tenant,
                status=result.status,
                detail={
                    "engine_version": result.engine_version,
                    "result": result.to_dict(),
                },
            )

        if job.kind == KIND_PROMPT_ANALYSIS:
            analysis = analyze_job(job, tenant_id=job_tenant, max_tokens=max_tokens)
            status = STATUS_FAILED if "error" in analysis else STATUS_OK
            return JobResult(
                job_id=job.id,
                kind=job.kind,
                tenant_id=job_tenant,
                status=status,
                detail={"analysis": analysis},
                error=analysis.get("error", ""),
            )

        if job.kind == KIND_REPLAY:
            from .replay import build_replay_plan, validate_replay_job

            errors = validate_replay_job(job)
            if errors:
                return JobResult(
                    job_id=job.id,
                    kind=job.kind,
                    tenant_id=job_tenant,
                    status=STATUS_FAILED,
                    error="; ".join(errors),
                )
            plan = build_replay_plan(job, max_requests=max_tokens or job.max_candidates or 100)
            # Score any embedded candidates as a convenience so a replay job
            # carrying outputs returns a ranking without a second round trip.
            detail: dict[str, Any] = {"plan": plan, "engine_version": ENGINE_VERSION}
            if job.candidates and job.reference:
                replay_result = evaluate_job(job, tenant_id=job_tenant)
                detail["evaluation"] = replay_result.to_dict()
            return JobResult(
                job_id=job.id,
                kind=job.kind,
                tenant_id=job_tenant,
                status=STATUS_OK,
                detail=detail,
            )

        # An unknown kind is a permanent failure: redelivering it will never help,
        # so it is reported rather than raised. The blanket handler below would
        # swallow a raise here anyway, so raising would have been a lie.
        return JobResult(
            job_id=job.id,
            kind=job.kind,
            tenant_id=job_tenant,
            status=STATUS_FAILED,
            error=f"unsupported job kind {job.kind!r}",
        )
    except Exception as exc:
        # Every failure is converted into a published result. A job that fails
        # silently would be retried until its delivery limit and then discarded,
        # leaving an operator with no record of why their evaluation never ran.
        return JobResult(
            job_id=job.id,
            kind=job.kind,
            tenant_id=job_tenant,
            status=STATUS_FAILED,
            error=f"{type(exc).__name__}: {exc}",
        )


def parse_job(payload: Mapping[str, Any]) -> EvalJob:
    """Parse a raw job payload."""
    job = EvalJob.from_dict(payload)
    if job.kind and job.kind not in KNOWN_JOB_KINDS:
        raise UnknownJobKindError(f"unsupported job kind {job.kind!r}")
    return job


def metrics_available() -> Sequence[str]:
    """List the scoring metrics a job may request."""
    from .scoring import SCORERS

    return tuple(sorted(SCORERS))
