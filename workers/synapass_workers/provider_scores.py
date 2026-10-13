"""Provider scoring rollups: explainable blends from usage events.

Scores combine success rate, latency, cost efficiency and feedback with fixed
weights, matching the Go scoring engine's semantics so worker rollups and
gateway decisions agree.
"""

from __future__ import annotations

from collections.abc import Iterable, Mapping

from .models import UsageEvent

DEFAULT_WEIGHTS = {"success": 0.5, "latency": 0.25, "cost": 0.15, "feedback": 0.1}


def _lat_score(avg_ms: float) -> float:
    return 1.0 / (1.0 + avg_ms / 2000.0)


def _cost_score(cost_per_success: float) -> float:
    return 1.0 / (1.0 + cost_per_success * 1000.0)


def score_providers(
    events: Iterable[UsageEvent],
    weights: Mapping[str, float] | None = None,
) -> list[dict]:
    """Aggregate usage events into per-provider scores, best first."""
    w = dict(DEFAULT_WEIGHTS)
    if weights:
        w.update(weights)
    total_w = sum(w.values()) or 1.0
    by_provider: dict[str, list[UsageEvent]] = {}
    for e in events:
        by_provider.setdefault(e.provider or "unknown", []).append(e)
    out: list[dict] = []
    for provider, items in by_provider.items():
        n = len(items)
        success = sum(1 for e in items if e.succeeded)
        success_rate = success / n if n else 0.0
        avg_lat = sum(e.latency_ms for e in items) / n if n else 0.0
        success_cost = [e.cost_usd for e in items if e.succeeded]
        cps = sum(success_cost) / len(success_cost) if success_cost else 0.0
        fallbacks = sum(1 for e in items if e.fallback_used) / n if n else 0.0
        score = (
            w["success"] * success_rate
            + w["latency"] * _lat_score(avg_lat)
            + w["cost"] * _cost_score(cps)
            + w["feedback"] * 0.5
        ) / total_w
        score *= 1.0 - 0.1 * fallbacks
        out.append(
            {
                "provider": provider,
                "requests": n,
                "success_rate": round(success_rate, 4),
                "avg_latency_ms": round(avg_lat, 1),
                "cost_per_success": round(cps, 6),
                "fallback_rate": round(fallbacks, 4),
                "score": round(score, 6),
                "explanation": (
                    f"success={success_rate:.2f} lat={avg_lat:.0f}ms cost/success=${cps:.4f} (n={n})"
                ),
            }
        )
    out.sort(key=lambda r: (-r["score"], r["provider"]))
    return out
