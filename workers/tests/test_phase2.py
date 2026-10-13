"""Phase 2 worker tests: replay, task classification, provider scores."""

import unittest

from synapass_workers.jobs import EvalJob, dispatch
from synapass_workers.models import KIND_REPLAY, UsageEvent
from synapass_workers.provider_scores import score_providers
from synapass_workers.replay import build_replay_plan, select_requests, validate_replay_job
from synapass_workers.task_classify import classify_task


class ReplayTest(unittest.TestCase):
    def test_validate_requires_ids(self):
        job = EvalJob(id="j1", kind=KIND_REPLAY, payload={})
        errors = validate_replay_job(job)
        self.assertTrue(errors)

    def test_plan_bounds(self):
        job = EvalJob(
            id="j1",
            kind=KIND_REPLAY,
            payload={"request_ids": [f"r{i}" for i in range(2000)], "providers": ["openai"]},
            max_candidates=50,
        )
        plan = build_replay_plan(job, max_requests=50)
        self.assertEqual(plan["request_count"], 50)

    def test_select_preserves_order(self):
        self.assertEqual(select_requests(["a", "b"], ["b", "a"], 10), ["b", "a"])

    def test_dispatch_replay_ok(self):
        job = EvalJob(
            id="j1",
            kind=KIND_REPLAY,
            payload={"request_ids": ["r1", "r2"], "providers": ["openai"]},
        )
        result = dispatch(job)
        self.assertEqual(result.status, "ok")
        self.assertIn("plan", result.detail)


class TaskClassifyTest(unittest.TestCase):
    def test_tool_use(self):
        req = {"messages": [{"role": "user", "content": "hi"}], "tools": [{"type": "function"}]}
        got = classify_task(req, 10)
        self.assertEqual(got["task"], "tool-use")

    def test_coding(self):
        req = {"messages": [{"role": "user", "content": "```python\ndef f(): pass"}]}
        got = classify_task(req, 10)
        self.assertEqual(got["task"], "coding")

    def test_long_context(self):
        req = {"messages": [{"role": "user", "content": "hello"}]}
        got = classify_task(req, 50000)
        self.assertEqual(got["task"], "long_context")

    def test_summarization(self):
        req = {"messages": [{"role": "user", "content": "summarize this article"}]}
        got = classify_task(req, 100)
        self.assertEqual(got["task"], "summarization")


class ProviderScoresTest(unittest.TestCase):
    def test_ranks_reliable_first(self):
        events = []
        for _ in range(10):
            events.append(UsageEvent(provider="good", outcome="success", latency_ms=100, cost_usd=0.001))
        for i in range(10):
            events.append(
                UsageEvent(
                    provider="bad",
                    outcome="success" if i < 2 else "error",
                    latency_ms=2000,
                    cost_usd=0.01,
                )
            )
        ranked = score_providers(events)
        self.assertEqual(ranked[0]["provider"], "good")


if __name__ == "__main__":
    unittest.main()
