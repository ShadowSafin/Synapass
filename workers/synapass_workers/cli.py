"""Command-line entrypoint.

Subcommands exist for the three things an operator actually does with this tier:

* ``serve``     run the consumers (the deployment entrypoint)
* ``score``     score a candidate against a reference, offline, to sanity-check a
                metric before wiring it into a job
* ``analyze``   analyse a prompt or a captured request, to see what routing would
                decide

The one-shot commands deliberately work with no broker and no third-party
dependency, so they are usable for debugging on the host that runs the gateway.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import Any

from . import ENGINE_VERSION, __version__
from .config import Config, ConfigError
from .jobs import dispatch
from .logging_setup import configure as configure_logging
from .metrics import build as build_metrics
from .models import EvalCandidate, EvalJob
from .prompt_analysis import analyze_request, analyze_text
from .scoring import SCORERS
from .telemetry import Aggregator


def build_parser() -> argparse.ArgumentParser:
    """Construct the argument parser."""
    parser = argparse.ArgumentParser(
        prog="synapass-worker",
        description="Synapass intelligence workers: evaluation, prompt analysis, telemetry rollups.",
    )
    parser.add_argument("--version", action="version", version=f"synapass-worker {__version__}")
    parser.add_argument("--config", help="path to a JSON or YAML configuration file")

    subparsers = parser.add_subparsers(dest="command", required=True)

    serve = subparsers.add_parser("serve", help="run the intelligence and telemetry consumers")
    serve.add_argument("--once", action="store_true", help="process the current backlog and exit")

    score = subparsers.add_parser("score", help="score one or more candidates against a reference")
    score.add_argument("--reference", required=True, help="the reference answer")
    score.add_argument("--reference-file", help="read the reference from a file instead")
    score.add_argument(
        "--candidate",
        action="append",
        default=[],
        metavar="ID=OUTPUT",
        help="a candidate answer; repeat for several",
    )
    score.add_argument(
        "--candidate-file",
        action="append",
        default=[],
        metavar="ID=PATH",
        help="read a candidate answer from a file",
    )
    score.add_argument("--metrics", help="comma-separated metric names")
    score.add_argument("--json", action="store_true", help="emit machine-readable output")

    analyze = subparsers.add_parser("analyze", help="analyse a prompt or a captured request")
    analyze.add_argument("--prompt", help="a bare prompt string")
    analyze.add_argument("--prompt-file", help="read the prompt from a file")
    analyze.add_argument("--request", help="a JSON file holding a chat completion request")
    analyze.add_argument("--max-tokens", type=int, default=0, help="flag prompts above this size")
    analyze.add_argument("--json", action="store_true", help="emit machine-readable output")

    metrics = subparsers.add_parser("metrics", help="list the available scoring metrics")
    metrics.add_argument("--json", action="store_true", help="emit machine-readable output")

    return parser


def main(argv: Sequence[str] | None = None) -> int:
    """Run the CLI, returning a process exit code."""
    parser = build_parser()
    args = parser.parse_args(argv)

    overrides: dict[str, Any] = {}
    if args.config:
        try:
            overrides = Config.from_file(args.config)
        except (OSError, ConfigError, ValueError) as exc:
            print(f"failed to read the config file: {exc}", file=sys.stderr)
            return 3

    try:
        config = Config.from_env().with_overrides(**overrides)
    except ConfigError as exc:
        print(f"invalid configuration: {exc}", file=sys.stderr)
        return 3

    configure_logging(
        level=config.log_level,
        fmt=config.log_format,
        labels=dict(config.labels),
    )

    if args.command == "serve":
        return _serve(config, once=bool(getattr(args, "once", False)))
    if args.command == "score":
        return _score(args)
    if args.command == "analyze":
        return _analyze(args)
    if args.command == "metrics":
        return _list_metrics(args)

    parser.error(f"unknown command {args.command!r}")  # pragma: no cover
    return 2


def _serve(config: Config, once: bool = False) -> int:
    """Run the worker consumers."""
    from .bus import Bus
    from .worker import Worker

    metrics = build_metrics(config.metrics_enabled)
    _ = metrics

    bus = Bus(
        config.nats_url,
        name=config.nats_name,
        credentials_file=config.nats_credentials_file,
        token=config.nats_token,
        jetstream=config.jetstream,
        max_deliver=config.max_deliver,
        ack_wait_seconds=config.ack_wait_seconds,
    )
    worker = Worker(config, bus=bus, aggregator=Aggregator(window_seconds=300), metrics=metrics)
    if once:
        # --once connects, drains the current backlog via the delivery threads, and
        # exits. It is the shape a Kubernetes Job or a cron run needs.
        worker.start()
        try:
            worker.publish_rollup()
        finally:
            worker.stop()
        return 0

    worker.run_forever()
    return 0


def _read_value(inline: str | None, path: str | None) -> str:
    """Read a value from either an inline argument or a file."""
    if path:
        return Path(path).read_text(encoding="utf-8").strip()
    return inline or ""


def _parse_pairs(values: Sequence[str], label: str) -> dict[str, str]:
    """Parse ``ID=VALUE`` arguments."""
    out: dict[str, str] = {}
    for raw in values:
        key, separator, value = raw.partition("=")
        if not separator or not key.strip():
            raise ValueError(f"{label} must be written as ID=VALUE, got {raw!r}")
        out[key.strip()] = value
    return out


def _score(args: argparse.Namespace) -> int:
    """Score candidates against a reference."""
    reference = _read_value(args.reference, args.reference_file)

    candidates: dict[str, str] = {}
    candidates.update(_parse_pairs(args.candidate, "candidate"))

    try:
        file_candidates = _parse_pairs(args.candidate_file, "candidate-file")
    except ValueError as exc:
        print(str(exc), file=sys.stderr)
        return 2
    for candidate_id, path in file_candidates.items():
        candidates[candidate_id] = Path(path).read_text(encoding="utf-8").strip()

    if not candidates:
        print("at least one candidate is required", file=sys.stderr)
        return 2

    metric_names = tuple(name.strip() for name in (args.metrics or "").split(",") if name.strip())
    try:
        job = EvalJob(
            id="cli-score",
            reference=reference,
            metrics=metric_names,
            candidates=tuple(
                EvalCandidate(id=candidate_id, output=output) for candidate_id, output in candidates.items()
            ),
        )
        result = dispatch(job)
    except ValueError as exc:
        print(f"scoring failed: {exc}", file=sys.stderr)
        return 2

    payload = result.detail.get("result", {})
    if args.json:
        print(json.dumps(payload, indent=2, sort_keys=True))
    else:
        print(f"reference: {len(reference)} chars")
        for entry in payload.get("ranking", []):
            per_metric = payload.get("scores", {}).get(entry["candidate_id"], {})
            rendered = " ".join(f"{name}={value:.4f}" for name, value in sorted(per_metric.items()))
            print(f"  {entry['candidate_id']:<24} composite={entry['score']:.4f}  {rendered}")
        if payload.get("winner"):
            print(f"winner: {payload['winner']}")
    return 0


def _analyze(args: argparse.Namespace) -> int:
    """Analyse a prompt or a captured request."""
    if args.request:
        try:
            request = json.loads(Path(args.request).read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError) as exc:
            print(f"failed to read the request file: {exc}", file=sys.stderr)
            return 2
        if not isinstance(request, Mapping):
            print("the request file must contain a JSON object", file=sys.stderr)
            return 2
        analysis = analyze_request(request, max_tokens=args.max_tokens)
    else:
        prompt = _read_value(args.prompt, args.prompt_file)
        if not prompt:
            print("provide --prompt, --prompt-file or --request", file=sys.stderr)
            return 2
        analysis = analyze_text(prompt, max_tokens=args.max_tokens)

    if args.json:
        print(json.dumps(analysis, indent=2, sort_keys=True))
    else:
        print(f"prompt tokens (estimated): {analysis['prompt_tokens']}")
        print(f"complexity: {analysis['complexity']}")
        print(f"capabilities: {', '.join(analysis['capabilities']) or 'none'}")
        print(f"suggested quality tier: {analysis['suggested_quality_tier']}")
        print(f"suggested strategy: {analysis['suggested_strategy']}")
        if analysis["sensitive_patterns"]:
            print(f"sensitive patterns: {', '.join(analysis['sensitive_patterns'])}")
        if analysis["flags"]:
            print(f"flags: {', '.join(analysis['flags'])}")
    return 0


def _list_metrics(args: argparse.Namespace) -> int:
    """List the registered scoring metrics."""
    names = sorted(SCORERS)
    if args.json:
        print(json.dumps({"engine_version": ENGINE_VERSION, "metrics": names}, indent=2))
    else:
        for name in names:
            print(name)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
