"""Deterministic offline scoring.

Every scorer here is a pure function of two strings. That is a deliberate
constraint, not a limitation: a score that cannot be reproduced byte-for-byte
from the same inputs cannot be used to compare two providers, because a
difference might be the model or might be the scorer.

Model-based judging exists separately and is explicitly not part of this module.
When a judge is used its verdict is recorded alongside the deterministic scores
with the judge's identity attached, so a regression in the judge is visible rather
than silently redefining what "better" means.

# Metric semantics

* ``exact_match``  -- 1.0 only for a normalised identical answer.
* ``token_f1``     -- harmonic mean of token precision and recall. The workhorse
                      for short answers.
* ``jaccard``      -- token-set overlap. Order-insensitive, so it is robust to a
                      model reordering a list.
* ``char_ngram``   -- cosine similarity over character trigrams. Tolerates
                      morphology and minor edits where whole-token metrics are
                      too harsh.
* ``length_ratio`` -- candidate length over reference length. Reported, never
                      used as a score on its own: a long answer is not better, it
                      is just longer.
"""

from __future__ import annotations

import math
import re
import unicodedata
from collections import Counter
from collections.abc import Callable, Iterable, Mapping, Sequence

# Scorers are registered by the name used on the wire in an evaluation job, so a
# job can request a metric without the caller knowing any Python.
SCORERS: dict[str, Scorer] = {}

_WHITESPACE = re.compile(r"\s+")
# Punctuation is stripped for normalised comparison but not for raw token counts:
# "USD 1,000" and "USD 1000" should match, while "no" and "no." are the same word.
_PUNCTUATION = re.compile(r"[^\w\s]", re.UNICODE)
_TOKEN = re.compile(r"\w+", re.UNICODE)


def _register(name: str) -> Callable[[Scorer], Scorer]:
    def decorator(scorer: Scorer) -> Scorer:
        SCORERS[name] = scorer
        return scorer

    return decorator


Scorer = Callable[[str, str], float]


def normalize_text(text: str) -> str:
    """Normalise a string for comparison.

    Unicode is normalised to NFKC so visually identical strings compare equal,
    case is folded, punctuation is dropped and whitespace is collapsed. The order
    matters: folding case before stripping punctuation would leave combining marks
    attached to removed characters on some inputs.
    """
    if not text:
        return ""
    folded = unicodedata.normalize("NFKC", text).casefold()
    stripped = _PUNCTUATION.sub(" ", folded)
    return _WHITESPACE.sub(" ", stripped).strip()


def tokenize(text: str) -> list[str]:
    """Split text into lowercase word tokens."""
    return _TOKEN.findall(unicodedata.normalize("NFKC", text).casefold())


def character_ngrams(text: str, n: int = 3) -> Counter[str]:
    """Count character n-grams over normalised text, with whitespace removed.

    Whitespace is dropped so that a re-wrapped answer is not penalised, which is
    common when the same content passes through two different templates.
    """
    if n < 1:
        raise ValueError("n must be at least 1")
    compact = normalize_text(text).replace(" ", "")
    if len(compact) < n:
        return Counter({compact: 1}) if compact else Counter()
    return Counter(compact[i : i + n] for i in range(len(compact) - n + 1))


@_register("exact_match")
def exact_match(reference: str, candidate: str) -> float:
    """1.0 when the normalised strings are identical, else 0.0."""
    if not reference and not candidate:
        return 1.0
    return 1.0 if normalize_text(reference) == normalize_text(candidate) else 0.0


def token_precision(reference: str, candidate: str) -> float:
    """Fraction of candidate tokens that also occur in the reference."""
    reference_tokens = tokenize(reference)
    candidate_tokens = tokenize(candidate)
    if not candidate_tokens:
        return 0.0
    reference_counts = Counter(reference_tokens)
    used = 0
    for token in candidate_tokens:
        if reference_counts[token] > 0:
            reference_counts[token] -= 1
            used += 1
    return used / len(candidate_tokens)


def token_recall(reference: str, candidate: str) -> float:
    """Fraction of reference tokens the candidate reproduced."""
    reference_tokens = tokenize(reference)
    if not reference_tokens:
        return 0.0
    return token_precision(candidate, reference)


@_register("token_f1")
def token_f1(reference: str, candidate: str) -> float:
    """Harmonic mean of token precision and recall.

    Counting with multiplicity rather than over sets is what makes this useful:
    an answer that repeats a token should not score as well as one that does not.
    """
    precision = token_precision(reference, candidate)
    recall = token_recall(reference, candidate)
    if precision + recall == 0:
        return 0.0
    return 2 * precision * recall / (precision + recall)


@_register("jaccard")
def jaccard(reference: str, candidate: str) -> float:
    """Intersection over union of token sets."""
    reference_tokens = set(tokenize(reference))
    candidate_tokens = set(tokenize(candidate))
    if not reference_tokens and not candidate_tokens:
        return 1.0
    union = reference_tokens | candidate_tokens
    if not union:
        return 0.0
    return len(reference_tokens & candidate_tokens) / len(union)


@_register("char_ngram")
def char_ngram_similarity(reference: str, candidate: str, n: int = 3) -> float:
    """Cosine similarity over character n-gram count vectors."""
    left = character_ngrams(reference, n)
    right = character_ngrams(candidate, n)
    if not left and not right:
        return 1.0
    if not left or not right:
        return 0.0

    shared = set(left) & set(right)
    dot = sum(left[gram] * right[gram] for gram in shared)
    if dot == 0:
        return 0.0
    left_norm = math.sqrt(sum(count * count for count in left.values()))
    right_norm = math.sqrt(sum(count * count for count in right.values()))
    return dot / (left_norm * right_norm)


@_register("length_ratio")
def length_ratio(reference: str, candidate: str) -> float:
    """Candidate length over reference length, capped at 1.0.

    Reported as a diagnostic rather than used as a quality score: a passing answer
    that is merely more verbose should not be rewarded for it, but a ratio far
    from one is a strong hint that something went wrong.
    """
    reference_length = len(reference.strip())
    candidate_length = len(candidate.strip())
    if reference_length == 0:
        return 1.0 if candidate_length == 0 else 0.0
    return min(candidate_length / reference_length, 1.0)


# Default weights used when a job does not specify any. They are named constants
# so a change to the composite score is a visible, reviewable edit rather than a
# literal buried in a function.
DEFAULT_WEIGHTS: Mapping[str, float] = {
    "token_f1": 0.6,
    "char_ngram": 0.3,
    "exact_match": 0.1,
}


def resolve_metrics(metrics: Sequence[str] | None) -> list[str]:
    """Validate and default a requested metric list."""
    if not metrics:
        return list(DEFAULT_WEIGHTS)
    unknown = [name for name in metrics if name not in SCORERS]
    if unknown:
        raise ValueError(f"unknown scoring metric(s): {', '.join(sorted(unknown))}")
    return list(metrics)


def score_candidate(reference: str, candidate: str, metrics: Sequence[str] | None = None) -> dict[str, float]:
    """Score one candidate against a reference across the requested metrics."""
    return {name: SCORERS[name](reference, candidate) for name in resolve_metrics(metrics)}


def composite_score(
    scores: Mapping[str, float],
    weights: Mapping[str, float] | None = None,
) -> float:
    """Combine per-metric scores into one number.

    Weights default to :data:`DEFAULT_WEIGHTS`. Metrics present in ``scores`` but
    absent from the weights contribute nothing rather than silently defaulting to
    a weight of one, which would let an unweighted diagnostic dominate the result.
    """
    weights = DEFAULT_WEIGHTS if weights is None else weights
    total_weight = 0.0
    total = 0.0
    for name, weight in weights.items():
        if weight <= 0 or name not in scores:
            continue
        total += scores[name] * weight
        total_weight += weight
    if total_weight == 0:
        return 0.0
    return round(total / total_weight, 6)


def rank_candidates(
    scored: Mapping[str, Mapping[str, float]],
    weights: Mapping[str, float] | None = None,
) -> list[tuple[str, float]]:
    """Rank candidates by composite score, best first.

    The sort is stable and ties are broken by candidate id, so the same job always
    produces the same ranking. A ranking that reshuffles between runs makes an A/B
    comparison between two providers impossible to interpret.
    """
    ranked = [(candidate_id, composite_score(scores, weights)) for candidate_id, scores in scored.items()]
    ranked.sort(key=lambda item: (-item[1], item[0]))
    return ranked


def percentile(values: Iterable[float], p: float) -> float:
    """Nearest-rank percentile, matching what the Go telemetry rollups report.

    Nearest-rank is used rather than interpolation because every other percentile
    in the platform is nearest-rank, and two different definitions of p95 in one
    dashboard produce numbers that appear to disagree.
    """
    if not values and values != []:  # pragma: no cover - defensive
        pass
    ordered = sorted(values)
    if not ordered:
        return 0.0
    if p <= 0:
        return float(ordered[0])
    if p >= 100:
        return float(ordered[-1])
    rank = math.ceil(p / 100.0 * len(ordered))
    return float(ordered[rank - 1])


def mean(values: Iterable[float]) -> float:
    """Arithmetic mean, or 0.0 for an empty input."""
    items = list(values)
    if not items:
        return 0.0
    return sum(items) / len(items)
