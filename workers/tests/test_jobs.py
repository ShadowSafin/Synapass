"""Tests for job dispatch, the runtime and configuration."""

from __future__ import annotations

import json
import unittest

from synapass_workers.bus import (
    SUBJECT_EVAL_RESULT,
    SUBJECT_PROMPT_ANALYSIS,
    SUBJECT_TELEMETRY_ROLLUP,
    BusError,
    NullBus,
    decode,
)
from synapass_workers.config import Config, ConfigError
from synapass_workers.jobs import UnknownJobKindError, analyze_job, dispatch, evaluate_job, parse_job
from synapass_workers.metrics import NullMetrics
from synapass_workers.models import (
    KIND_EVAL,
    KIND_PROMPT_ANALYSIS,
    KIND_REPLAY,
    STATUS_FAILED,
    STATUS_OK,
    STATUS_SKIPPED,
    EvalJob,
)
from synapass_workers.telemetry import Aggregator
from synapass_workers.worker import Worker


def eval_payload(**overrides):
    """Build a raw evaluation job payload."""
    payload = {
        "id": "job-1",
        "kind": "eval",
        "tenant_id": "t-1",
        "reference": "Paris is the capital of France.",
        "candidates": [
            {"id": "a", "provider": "openai", "model": "gpt-4o", "output": "Paris is the capital of France."},
            {
                "id": "b",
                "provider": "anthropic",
                "model": "claude",
                "output": "Lyon is the capital of France.",
            },
        ],
    }
    payload.update(overrides)
    return payload


class TestParsing(unittest.TestCase):
    def test_parses_a_full_job(self) -> None:
        job = parse_job(eval_payload(metrics=["token_f1"], weights={"token_f1": 1.0}))
        self.assertEqual(job.id, "job-1")
        self.assertEqual(job.kind, KIND_EVAL)
        self.assertEqual(job.tenant_id, "t-1")
        self.assertEqual(job.metrics, ("token_f1",))
        self.assertEqual(job.weights, {"token_f1": 1.0})
        self.assertEqual(len(job.candidates), 2)

    def test_unknown_keys_are_ignored(self) -> None:
        # A control plane one release ahead must not break a running worker.
        job = parse_job(eval_payload(a_field_from_the_future={"x": 1}))
        self.assertEqual(job.id, "job-1")

    def test_missing_optional_fields_use_defaults(self) -> None:
        job = parse_job({"id": "j"})
        self.assertEqual(job.kind, KIND_EVAL)
        self.assertEqual(job.candidates, ())
        self.assertEqual(job.max_candidates, 64)

    def test_unsupported_kind_is_rejected(self) -> None:
        with self.assertRaises(UnknownJobKindError):
            parse_job({"id": "j", "kind": "summon_demon"})

    def test_job_id_falls_back_to_job_id_key(self) -> None:
        job = parse_job({"job_id": "legacy-id"})
        self.assertEqual(job.id, "legacy-id")

    def test_candidate_id_falls_back_to_the_model_name(self) -> None:
        job = parse_job({"id": "j", "candidates": [{"model": "gpt-4o", "output": "x"}]})
        self.assertEqual(job.candidates[0].id, "gpt-4o")


class TestEvaluateJob(unittest.TestCase):
    def test_ranks_the_better_answer_first(self) -> None:
        job = parse_job(eval_payload())
        result = evaluate_job(job)

        self.assertEqual(result.status, STATUS_OK)
        self.assertEqual(result.winner, "a")
        self.assertEqual(len(result.ranking), 2)
        self.assertGreater(result.ranking[0][1], result.ranking[1][1])
        self.assertTrue(result.engine_version)

    def test_scores_are_recorded_per_metric(self) -> None:
        job = parse_job(eval_payload(metrics=["token_f1", "jaccard"]))
        result = evaluate_job(job)
        self.assertEqual(set(result.scores["a"]), {"token_f1", "jaccard"})

    def test_failed_candidates_are_skipped_not_scored(self) -> None:
        # Scoring an error message against a reference would enter a zero into an
        # average and quietly drag a provider's reported quality down.
        job = parse_job(
            eval_payload(
                candidates=[
                    {"id": "ok", "output": "Paris is the capital of France."},
                    {"id": "broken", "output": "", "error": "upstream_error: connection reset"},
                ]
            )
        )
        result = evaluate_job(job)

        self.assertIn("broken", result.skipped)
        self.assertNotIn("broken", result.scores)
        self.assertEqual(result.winner, "ok")

    def test_failed_candidates_can_be_scored_on_request(self) -> None:
        job = parse_job(
            eval_payload(
                candidates=[
                    {"id": "broken", "output": "", "error": "upstream_error"},
                ]
            )
        )
        result = evaluate_job(job, include_failures=True)
        self.assertIn("broken", result.scores)
        self.assertEqual(result.scores["broken"]["token_f1"], 0.0)

    def test_empty_output_is_scored_as_zero_and_noted(self) -> None:
        job = parse_job(eval_payload(candidates=[{"id": "empty", "output": ""}]))
        result = evaluate_job(job)
        self.assertEqual(result.winner, "empty")
        self.assertEqual(result.scores["empty"]["token_f1"], 0.0)
        self.assertTrue(any("no output" in note for note in result.notes))

    def test_a_job_with_no_scoreable_candidates_is_skipped(self) -> None:
        job = parse_job(
            eval_payload(
                candidates=[
                    {"id": "broken", "error": "upstream_error"},
                ]
            )
        )
        result = evaluate_job(job)
        self.assertEqual(result.status, STATUS_SKIPPED)
        self.assertEqual(result.ranking, ())
        self.assertTrue(any("no candidate" in note for note in result.notes))

    def test_candidate_count_is_bounded(self) -> None:
        payload = eval_payload(max_candidates=2)
        payload["candidates"] = [
            {"id": f"c{i}", "output": "Paris is the capital of France."} for i in range(10)
        ]
        result = evaluate_job(parse_job(payload))

        self.assertEqual(len(result.scores), 2)
        self.assertEqual(result.candidate_count, 2)
        self.assertTrue(any("only the first 2" in note for note in result.notes))

    def test_empty_candidate_list_is_skipped(self) -> None:
        result = evaluate_job(parse_job(eval_payload(candidates=[])))
        self.assertEqual(result.status, STATUS_SKIPPED)

    def test_result_serialises_to_the_documented_shape(self) -> None:
        result = evaluate_job(parse_job(eval_payload()))
        payload = result.to_dict()
        for key in ("job_id", "tenant_id", "status", "engine_version", "ranking", "scores", "winner"):
            self.assertIn(key, payload)
        self.assertEqual(payload["ranking"][0]["candidate_id"], "a")
        # The rendered payload must itself be JSON-serialisable, since it is
        # published over the bus.
        json.dumps(payload)


class TestAnalyzeJob(unittest.TestCase):
    def test_analyses_an_embedded_request(self) -> None:
        job = parse_job(
            {
                "id": "j",
                "kind": KIND_PROMPT_ANALYSIS,
                "request_id": "req-1",
                "request": {
                    "model": "gpt-4o",
                    "messages": [{"role": "user", "content": "hello"}],
                },
            }
        )
        analysis = analyze_job(job)
        self.assertEqual(analysis["request_id"], "req-1")
        self.assertIn("chat", analysis["capabilities"])
        self.assertTrue(analysis["engine_version"])

    def test_analyses_a_bare_prompt(self) -> None:
        job = parse_job({"id": "j", "kind": KIND_PROMPT_ANALYSIS, "prompt": "explain DNS"})
        analysis = analyze_job(job)
        self.assertGreater(analysis["prompt_tokens"], 0)

    def test_reports_a_job_with_neither_shape(self) -> None:
        job = parse_job({"id": "j", "kind": KIND_PROMPT_ANALYSIS})
        analysis = analyze_job(job)
        self.assertIn("error", analysis)


class TestDispatch(unittest.TestCase):
    def test_routes_an_evaluation_job(self) -> None:
        result = dispatch(parse_job(eval_payload()))
        self.assertEqual(result.status, STATUS_OK)
        self.assertEqual(result.kind, KIND_EVAL)
        self.assertIn("result", result.detail)

    def test_routes_a_prompt_analysis_job(self) -> None:
        result = dispatch(
            parse_job(
                {
                    "id": "j",
                    "kind": KIND_PROMPT_ANALYSIS,
                    "request": {"messages": [{"role": "user", "content": "hi"}]},
                }
            )
        )
        self.assertEqual(result.status, STATUS_OK)
        self.assertIn("analysis", result.detail)

    def test_replay_is_skipped_not_failed(self) -> None:
        # Phase 2 implements replay: a job without ids fails validation (a
        # visible failure), while a well-formed job returns a plan.
        result = dispatch(parse_job({"id": "j", "kind": KIND_REPLAY}))
        self.assertEqual(result.status, STATUS_FAILED)
        result2 = dispatch(
            parse_job(
                {
                    "id": "j2",
                    "kind": KIND_REPLAY,
                    "request_ids": ["r1"],
                    "providers": ["openai"],
                }
            )
        )
        self.assertEqual(result2.status, STATUS_OK)
        self.assertIn("plan", result2.detail)

    def test_an_unexpected_failure_becomes_a_published_result(self) -> None:
        # Every failure must produce a record; a job that fails silently gets
        # retried until its delivery limit and then disappears.
        job = EvalJob(id="j", kind=KIND_EVAL, reference="x", metrics=("nonexistent_metric",))
        result = dispatch(job)
        self.assertEqual(result.status, STATUS_FAILED)
        self.assertIn("ValueError", result.error)

    def test_tenant_override_wins(self) -> None:
        result = dispatch(parse_job(eval_payload()), tenant_id="override")
        self.assertEqual(result.tenant_id, "override")

    def test_unknown_kind_is_reported_as_a_permanent_failure(self) -> None:
        # Reported rather than raised: redelivery cannot fix an unknown kind, and a
        # raise from here would be swallowed by the envelope's own error boundary.
        result = dispatch(EvalJob(id="j", kind="summon_demon"))
        self.assertEqual(result.status, STATUS_FAILED)
        self.assertIn("unsupported job kind", result.error)


class TestNullBusAndDecode(unittest.TestCase):
    def test_null_bus_records_publications(self) -> None:
        bus = NullBus()
        bus.publish_json("ar.test", {"a": 1})
        self.assertEqual(bus.messages_for("ar.test"), [{"a": 1}])
        self.assertTrue(bus.connected)

    def test_decode_rejects_non_json(self) -> None:
        with self.assertRaises(BusError):
            decode(b"not json at all")

    def test_decode_rejects_a_non_object(self) -> None:
        with self.assertRaises(BusError):
            decode(b"[1, 2, 3]")

    def test_decode_accepts_an_object(self) -> None:
        self.assertEqual(decode(b'{"a": 1}'), {"a": 1})


class TestWorkerRuntime(unittest.TestCase):
    def setUp(self) -> None:
        self.config = Config()
        self.bus = NullBus()
        self.worker = Worker(
            self.config,
            bus=self.bus,
            aggregator=Aggregator(),
            metrics=NullMetrics(),
        )

    def test_handles_an_evaluation_job_and_publishes_a_result(self) -> None:
        self.worker.handle_job_message(json.dumps(eval_payload()).encode())

        published = self.bus.messages_for(SUBJECT_EVAL_RESULT)
        self.assertEqual(len(published), 1)
        self.assertEqual(published[0]["job_id"], "job-1")
        self.assertEqual(self.worker.stats.jobs_handled, 1)
        self.assertEqual(self.worker.stats.jobs_failed, 0)

    def test_routes_a_prompt_analysis_result_to_its_own_subject(self) -> None:
        self.worker.handle_job_message(
            json.dumps(
                {
                    "id": "j",
                    "kind": KIND_PROMPT_ANALYSIS,
                    "request": {"messages": [{"role": "user", "content": "hi"}]},
                }
            ).encode()
        )

        self.assertEqual(len(self.bus.messages_for(SUBJECT_PROMPT_ANALYSIS)), 1)
        self.assertEqual(len(self.bus.messages_for(SUBJECT_EVAL_RESULT)), 0)

    def test_a_malformed_payload_is_discarded_without_redelivery(self) -> None:
        # The bytes will never parse, so asking for redelivery would burn the
        # delivery budget and produce nothing.
        self.worker.handle_job_message(b"{not json")

        self.assertEqual(self.worker.stats.decode_failures, 1)
        self.assertEqual(self.worker.stats.jobs_handled, 0)
        self.assertEqual(self.bus.messages_for(SUBJECT_EVAL_RESULT), [])

    def test_a_failing_job_is_counted_and_still_publishes_a_result(self) -> None:
        self.worker.handle_job_message(
            json.dumps(
                {
                    "id": "j",
                    "kind": "eval",
                    "reference": "x",
                    "metrics": ["nonexistent_metric"],
                    "candidates": [{"id": "a", "output": "x"}],
                }
            ).encode()
        )

        self.assertEqual(self.worker.stats.jobs_failed, 1)
        published = self.bus.messages_for(SUBJECT_EVAL_RESULT)
        self.assertEqual(published[0]["status"], STATUS_FAILED)

    def test_usage_events_feed_the_aggregator(self) -> None:
        self.worker.handle_usage_message(
            json.dumps(
                {
                    "provider": "openai",
                    "model": "gpt-4o",
                    "outcome": "success",
                    "latency_ms": 120,
                    "cost": {"usd": 0.002},
                    "usage": {"prompt_tokens": 10, "completion_tokens": 4},
                }
            ).encode()
        )

        self.assertEqual(self.worker.stats.usage_events, 1)
        entry = self.worker.aggregator.snapshot()[0]
        self.assertEqual(entry["requests"], 1)
        self.assertAlmostEqual(entry["cost_usd"], 0.002)
        self.assertEqual(entry["total_tokens"], 14)

    def test_publishes_a_rollup(self) -> None:
        self.worker.handle_usage_message(
            json.dumps(
                {
                    "provider": "openai",
                    "model": "gpt-4o",
                    "outcome": "success",
                }
            ).encode()
        )
        payload = self.worker.publish_rollup()

        self.assertIn("providers", payload)
        self.assertEqual(self.worker.stats.rollups_published, 1)
        published = self.bus.messages_for(SUBJECT_TELEMETRY_ROLLUP)
        self.assertEqual(len(published), 1)

    def test_health_reports_the_bus_and_config(self) -> None:
        self.worker.stats.started_at = 1.0
        health = self.worker.health()

        self.assertEqual(health["status"], "ok")
        self.assertTrue(health["bus"]["connected"])
        self.assertIn("config", health)
        # Secrets must never appear in a health payload.
        self.assertNotIn(
            "provider_api_key", json.dumps(health["config"]).replace('"provider_api_key": ""', "")
        )

    def test_stop_is_idempotent(self) -> None:
        self.worker.stop()
        self.worker.stop()


class TestConfiguration(unittest.TestCase):
    def test_defaults_are_usable(self) -> None:
        config = Config()
        self.assertTrue(config.metrics_enabled)
        self.assertFalse(config.judging_enabled)
        self.assertEqual(config.queue_group, "synapass-workers")

    def test_environment_overrides(self) -> None:
        config = Config.from_env(
            {
                "SYNAPASS_WORKER_NATS_URL": "nats://nats:4222",
                "SYNAPASS_WORKER_METRICS_ADDR": "127.0.0.1:9200",
                "SYNAPASS_WORKER_LOG_LEVEL": "debug",
                "SYNAPASS_WORKER_EVAL_CONCURRENCY": "8",
                "SYNAPASS_WORKER_METRICS_ENABLED": "false",
                "SYNAPASS_WORKER_LABELS": "pool=eval,region=eu",
            }
        )

        self.assertEqual(config.nats_url, "nats://nats:4222")
        self.assertEqual(config.metrics_addr, "127.0.0.1:9200")
        self.assertEqual(config.log_level, "debug")
        self.assertEqual(config.eval_concurrency, 8)
        self.assertFalse(config.metrics_enabled)
        self.assertEqual(dict(config.labels), {"pool": "eval", "region": "eu"})

    def test_falls_back_to_plain_nats_url(self) -> None:
        config = Config.from_env({"NATS_URL": "nats://shared:4222"})
        self.assertEqual(config.nats_url, "nats://shared:4222")

    def test_rejects_a_malformed_boolean(self) -> None:
        with self.assertRaises(ConfigError):
            Config.from_env({"SYNAPASS_WORKER_METRICS_ENABLED": "perhaps"})

    def test_rejects_a_non_numeric_integer(self) -> None:
        with self.assertRaises(ConfigError):
            Config.from_env({"SYNAPASS_WORKER_EVAL_CONCURRENCY": "many"})

    def test_rejects_a_zero_concurrency(self) -> None:
        with self.assertRaises(ConfigError):
            Config.from_env({"SYNAPASS_WORKER_EVAL_CONCURRENCY": "0"})

    def test_rejects_an_unknown_log_level(self) -> None:
        with self.assertRaises(ConfigError):
            Config.from_env({"SYNAPASS_WORKER_LOG_LEVEL": "chatty"})

    def test_rejects_a_judge_endpoint_without_a_model(self) -> None:
        with self.assertRaises(ConfigError):
            Config(provider_url="http://localhost:11434/v1")

    def test_rejects_malformed_labels(self) -> None:
        with self.assertRaises(ConfigError):
            Config.from_env({"SYNAPASS_WORKER_LABELS": "pool"})

    def test_redacts_secrets_when_rendered(self) -> None:
        config = Config(
            nats_token="super-secret",
            provider_api_key="sk-secret",
            provider_url="http://localhost:11434/v1",
            provider_model="llama3",
        )
        rendered = config.to_dict(redact=True)

        self.assertEqual(rendered["nats_token"], "<set>")
        self.assertEqual(rendered["provider_api_key"], "<set>")
        self.assertNotIn("super-secret", json.dumps(rendered))
        self.assertNotIn("sk-secret", json.dumps(rendered))
        self.assertTrue(rendered["judging_enabled"])

    def test_with_overrides_returns_a_copy(self) -> None:
        base = Config()
        updated = base.with_overrides(log_level="debug")
        self.assertEqual(base.log_level, "info")
        self.assertEqual(updated.log_level, "debug")


if __name__ == "__main__":
    unittest.main()
