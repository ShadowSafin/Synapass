"""Optional offline judge model client.

A judge is a model asked to rate an answer, used when the reference is a rubric
rather than a string. It is strictly optional and strictly local: judging a
candidate set with a hosted provider would make the eval worker depend on the very
providers it exists to measure, and would put evaluation cost onto the bill it is
trying to reduce.

The deterministic scorers in :mod:`scoring` remain the primary signal. A judge
verdict is recorded alongside them with the judge's identity attached, so a change
in the judge is visible rather than silently redefining what "better" means.
"""

from __future__ import annotations

import json
import logging
from collections.abc import Mapping
from typing import Any

logger = logging.getLogger(__name__)

# The rubric is constrained to a single number on purpose. A judge asked for prose
# produces reasoning that has to be parsed, and a parse failure would silently
# become a zero; asking for one integer removes that failure mode entirely.
JUDGE_SYSTEM_PROMPT = (
    "You are an exacting but fair evaluator. You are given a reference answer and a "
    "candidate answer. Rate how well the candidate matches the reference on a scale "
    "from 0 to 10, where 0 is unrelated and 10 is fully correct and complete. "
    'Reply with a single JSON object: {"score": <number>, "rationale": "<one sentence>"}. '
    "Reply with nothing else."
)

# A judge that returns something outside the scale is clamped rather than rejected:
# the verdict is advisory and a clipped score is more useful than no score.
MIN_SCORE = 0.0
MAX_SCORE = 10.0


class JudgeUnavailableError(RuntimeError):
    """Raised when the judge endpoint is not configured or not reachable."""


class JudgeClient:
    """A minimal OpenAI-compatible chat client used as an offline judge."""

    def __init__(
        self,
        base_url: str,
        model: str,
        *,
        api_key: str = "",
        timeout_seconds: float = 60.0,
    ) -> None:
        if not base_url or not model:
            raise JudgeUnavailableError("judge base_url and model must both be configured")
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.api_key = api_key
        self.timeout_seconds = timeout_seconds

    def judge(self, reference: str, candidate: str) -> dict[str, Any]:
        """Ask the judge to score one candidate.

        Returns a mapping with ``score`` in the 0..10 range, ``normalised`` in
        0..1 so it can be compared with the deterministic metrics, ``rationale``,
        ``model`` and ``engine``. A transport or parse failure raises
        :class:`JudgeUnavailableError` so the caller can record that no verdict was
        obtained, rather than recording a zero.
        """
        import httpx

        payload = {
            "model": self.model,
            "temperature": 0,
            # A judge must not be creative. Temperature zero plus a single-integer
            # request is what makes a judge reproducible enough to compare runs.
            "messages": [
                {"role": "system", "content": JUDGE_SYSTEM_PROMPT},
                {
                    "role": "user",
                    "content": (
                        f"Reference answer:\n{reference}\n\n"
                        f"Candidate answer:\n{candidate}\n\n"
                        "Rate the candidate."
                    ),
                },
            ],
        }

        headers = {"Content-Type": "application/json"}
        if self.api_key:
            headers["Authorization"] = f"Bearer {self.api_key}"

        try:
            with httpx.Client(timeout=self.timeout_seconds) as client:
                response = client.post(
                    f"{self.base_url}/chat/completions",
                    json=payload,
                    headers=headers,
                )
                response.raise_for_status()
                body = response.json()
        except Exception as exc:
            raise JudgeUnavailableError(f"judge request failed: {exc}") from exc

        content = _first_content(body)
        if not content:
            raise JudgeUnavailableError("the judge returned no content")

        parsed = _parse_verdict(content)
        raw_score = _clamp(float(parsed.get("score", 0.0)))
        return {
            "score": raw_score,
            "normalised": round(raw_score / MAX_SCORE, 6),
            "rationale": str(parsed.get("rationale", ""))[:512],
            "model": self.model,
            "engine": "judge-v1",
        }

    def available(self) -> bool:
        """Report whether the judge can be reached.

        A cheap unauthenticated GET on the models endpoint is used rather than
        spending a completion: this is called at startup to decide whether judging
        is wired up, not to validate the model.
        """
        import httpx

        try:
            with httpx.Client(timeout=min(self.timeout_seconds, 5.0)) as client:
                response = client.get(f"{self.base_url}/models")
            return response.status_code < 500
        except Exception:
            return False


def _first_content(body: Mapping[str, Any]) -> str:
    """Extract the first choice's message content."""
    choices = body.get("choices")
    if not isinstance(choices, list) or not choices:
        return ""
    first = choices[0]
    if not isinstance(first, Mapping):
        return ""
    message = first.get("message")
    if not isinstance(message, Mapping):
        return ""
    content = message.get("content")
    if isinstance(content, str):
        return content
    # Some servers return a part list. The text parts are joined, skipping the
    # reasoning parts that a thinking model leaks into the same field.
    if isinstance(content, list):
        return "\n".join(
            str(part.get("text", ""))
            for part in content
            if isinstance(part, Mapping) and part.get("type") in {"text", None}
        )
    return ""


def _parse_verdict(content: str) -> Mapping[str, Any]:
    """Parse the judge's JSON verdict, tolerating a fenced code block.

    Local models routinely wrap JSON in a markdown fence despite being asked not
    to. Discarding an otherwise valid verdict over a fence would make judging look
    far less reliable than it is.
    """
    text = content.strip()
    if text.startswith("```"):
        text = text.split("```")[1] if "```" in text[3:] else text[3:]
        if text.lstrip().startswith("json"):
            text = text.lstrip()[4:]
        text = text.strip()

    try:
        parsed = json.loads(text)
    except json.JSONDecodeError:
        # Fall back to the first integer found anywhere in the reply. This is the
        # difference between a usable verdict and a lost one for a model that
        # answers "Score: 8" instead of the requested object.
        import re

        match = re.search(r"-?\d+(?:\.\d+)?", text)
        if not match:
            raise JudgeUnavailableError(f"the judge reply contained no score: {content[:200]!r}") from None
        return {"score": float(match.group(0)), "rationale": text[:200]}

    if not isinstance(parsed, Mapping):
        raise JudgeUnavailableError("the judge reply was not a JSON object")
    return parsed


def _clamp(value: float) -> float:
    """Clamp a judge score into the documented range."""
    return max(MIN_SCORE, min(MAX_SCORE, value))
