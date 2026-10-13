"""Tests for prompt analysis."""

from __future__ import annotations

import unittest

from synapass_workers.prompt_analysis import (
    LONG_CONTEXT_TOKENS,
    analyze_request,
    analyze_text,
    classify_complexity,
    contains_code,
    content_has_image,
    detect_capabilities,
    estimate_request_tokens,
    estimate_tokens,
    luhn_valid,
    non_ascii_ratio,
    sensitive_patterns,
    wants_json,
)


def request(**overrides):
    """Build a minimal chat completion request."""
    base = {
        "model": "gpt-4o",
        "messages": [{"role": "user", "content": "hello"}],
    }
    base.update(overrides)
    return base


class TestTokenEstimation(unittest.TestCase):
    def test_empty_text_has_no_tokens(self) -> None:
        self.assertEqual(estimate_tokens(""), 0)
        self.assertEqual(estimate_tokens("    "), 0)

    def test_short_text_still_costs_a_token(self) -> None:
        # A character-ratio estimator returns zero for "hi", which would make an
        # empty prompt look free.
        self.assertEqual(estimate_tokens("hi"), 1)

    def test_longer_text_scales_with_length(self) -> None:
        self.assertGreater(estimate_tokens("x" * 3600), estimate_tokens("x" * 360))

    def test_request_estimation_includes_message_framing(self) -> None:
        one = estimate_request_tokens({"messages": [{"role": "user", "content": "hi"}]})
        ten = estimate_request_tokens({"messages": [{"role": "user", "content": "hi"}] * 10})
        self.assertGreater(ten, one)

    def test_tool_declarations_are_counted(self) -> None:
        without = estimate_request_tokens(request())
        with_tools = estimate_request_tokens(
            request(
                tools=[
                    {
                        "type": "function",
                        "function": {
                            "name": "search_documents",
                            "description": "Search the document store for a query string",
                            "parameters": {"type": "object"},
                        },
                    }
                ]
            )
        )
        self.assertGreater(with_tools, without)

    def test_tool_call_arguments_are_counted(self) -> None:
        with_call = estimate_request_tokens(
            request(
                messages=[
                    {
                        "role": "assistant",
                        "content": "",
                        "tool_calls": [
                            {
                                "id": "call_1",
                                "type": "function",
                                "function": {
                                    "name": "search_documents",
                                    "arguments": '{"query":"a very long query"}',
                                },
                            }
                        ],
                    }
                ]
            )
        )
        self.assertGreater(with_call, 0)


class TestCapabilityDetection(unittest.TestCase):
    def test_plain_chat_requires_only_chat(self) -> None:
        self.assertEqual(detect_capabilities(request()), ("chat",))

    def test_streaming_is_detected(self) -> None:
        self.assertIn("streaming", detect_capabilities(request(stream=True)))

    def test_tools_and_parallel_calls_are_detected(self) -> None:
        capabilities = detect_capabilities(
            request(
                tools=[{"type": "function", "function": {"name": "f", "parameters": {}}}],
                parallel_tool_calls=True,
            )
        )
        self.assertIn("tools", capabilities)
        self.assertIn("parallel_tools", capabilities)

    def test_vision_is_detected_from_content_parts(self) -> None:
        capabilities = detect_capabilities(
            request(
                messages=[
                    {
                        "role": "user",
                        "content": [
                            {"type": "text", "text": "what is this?"},
                            {"type": "image_url", "image_url": {"url": "https://example.invalid/a.png"}},
                        ],
                    }
                ]
            )
        )
        self.assertIn("vision", capabilities)

    def test_json_schema_implies_json_mode(self) -> None:
        capabilities = detect_capabilities(
            request(response_format={"type": "json_schema", "json_schema": {"name": "x"}})
        )
        self.assertIn("json_schema", capabilities)
        self.assertIn("json_mode", capabilities)

    def test_json_object_does_not_imply_a_schema(self) -> None:
        capabilities = detect_capabilities(request(response_format={"type": "json_object"}))
        self.assertIn("json_mode", capabilities)
        self.assertNotIn("json_schema", capabilities)

    def test_reasoning_effort_is_detected(self) -> None:
        self.assertIn("reasoning", detect_capabilities(request(reasoning_effort="high")))
        self.assertNotIn("reasoning", detect_capabilities(request(reasoning_effort="none")))

    def test_capabilities_are_sorted_for_stable_output(self) -> None:
        capabilities = detect_capabilities(
            request(
                stream=True,
                tools=[
                    {"type": "function", "function": {"name": "f", "parameters": {}}},
                ],
            )
        )
        self.assertEqual(list(capabilities), sorted(capabilities))


class TestContentHelpers(unittest.TestCase):
    def test_image_detection_handles_both_part_spellings(self) -> None:
        self.assertTrue(content_has_image([{"type": "image"}]))
        self.assertTrue(content_has_image([{"type": "input_image"}]))
        self.assertFalse(content_has_image([{"type": "text", "text": "hi"}]))

    def test_image_detection_ignores_plain_strings(self) -> None:
        self.assertFalse(content_has_image("a string with the word image in it"))

    def test_json_intent_detection(self) -> None:
        self.assertTrue(wants_json(request(response_format={"type": "json_object"})))
        self.assertFalse(wants_json(request()))


class TestCodeDetection(unittest.TestCase):
    def test_fenced_code_is_detected(self) -> None:
        self.assertTrue(contains_code("Here you go:\n```python\nprint(1)\n```"))

    def test_language_keywords_are_detected(self) -> None:
        self.assertTrue(contains_code("def handler(event): return event"))

    def test_prose_is_not_code(self) -> None:
        self.assertFalse(contains_code("Please summarise the attached report for me."))


class TestSensitivePatterns(unittest.TestCase):
    def test_email_is_flagged(self) -> None:
        self.assertIn("email", sensitive_patterns("contact ada@example.com today"))

    def test_api_key_shaped_strings_are_flagged(self) -> None:
        self.assertIn("api_key", sensitive_patterns("use sk-abcdefghijklmnopqrstuvwx please"))

    def test_bearer_tokens_are_flagged(self) -> None:
        self.assertIn(
            "bearer_token",
            sensitive_patterns("Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"),
        )

    def test_government_ids_are_flagged(self) -> None:
        self.assertIn("government_id", sensitive_patterns("ssn 123-45-6789"))

    def test_luhn_valid_card_numbers_are_flagged(self) -> None:
        # 4111 1111 1111 1111 passes Luhn; the checksum is what keeps ordinary
        # 16-digit identifiers from raising a flag nobody trusts.
        self.assertIn("credit_card", sensitive_patterns("card 4111 1111 1111 1111 on file"))

    def test_random_digit_runs_are_not_flagged_as_cards(self) -> None:
        self.assertNotIn("credit_card", sensitive_patterns("order id 1234567890123456"))

    def test_luhn_rejects_the_wrong_length(self) -> None:
        self.assertFalse(luhn_valid("411111111111"))
        self.assertFalse(luhn_valid("4" * 25))

    def test_clean_text_has_no_patterns(self) -> None:
        self.assertEqual(sensitive_patterns("explain how DNS resolution works"), ())


class TestComplexity(unittest.TestCase):
    def test_short_single_turn_is_trivial(self) -> None:
        self.assertEqual(
            classify_complexity(
                50, has_tools=False, has_code=False, has_image=False, message_count=1, reasoning=False
            ),
            "trivial",
        )

    def test_tools_escalate_to_complex(self) -> None:
        # Tool use needs a model that can follow a schema, which is a quality
        # requirement rather than a size one.
        self.assertEqual(
            classify_complexity(
                100, has_tools=True, has_code=False, has_image=False, message_count=1, reasoning=False
            ),
            "complex",
        )

    def test_code_or_images_escalate_to_moderate(self) -> None:
        self.assertEqual(
            classify_complexity(
                500, has_tools=False, has_code=True, has_image=False, message_count=1, reasoning=False
            ),
            "moderate",
        )
        self.assertEqual(
            classify_complexity(
                500, has_tools=False, has_code=False, has_image=True, message_count=1, reasoning=False
            ),
            "moderate",
        )

    def test_long_prompts_escalate_to_complex(self) -> None:
        self.assertEqual(
            classify_complexity(
                LONG_CONTEXT_TOKENS + 1,
                has_tools=False,
                has_code=False,
                has_image=False,
                message_count=1,
                reasoning=False,
            ),
            "complex",
        )

    def test_many_turns_escalate_to_moderate(self) -> None:
        self.assertEqual(
            classify_complexity(
                400, has_tools=False, has_code=False, has_image=False, message_count=8, reasoning=False
            ),
            "moderate",
        )


class TestAnalyzeRequest(unittest.TestCase):
    def test_returns_a_complete_analysis(self) -> None:
        analysis = analyze_request(request(stream=True))
        for key in (
            "prompt_tokens",
            "capabilities",
            "complexity",
            "suggested_quality_tier",
            "suggested_strategy",
            "flags",
            "engine_version",
        ):
            self.assertIn(key, analysis)
        self.assertIn("chat", analysis["capabilities"])
        self.assertIn("streaming", analysis["capabilities"])
        self.assertTrue(analysis["engine_version"])

    def test_long_prompt_gains_the_long_context_capability(self) -> None:
        # Routing to a small-window model would simply fail, so the requirement is
        # a capability rather than a preference.
        analysis = analyze_request(
            request(messages=[{"role": "user", "content": "x" * (LONG_CONTEXT_TOKENS * 4)}])
        )
        self.assertIn("long_context", analysis["capabilities"])
        self.assertEqual(analysis["complexity"], "complex")

    def test_cheap_requests_suggest_the_cost_strategy(self) -> None:
        analysis = analyze_request(request())
        self.assertEqual(analysis["suggested_strategy"], "lowest_cost")
        self.assertEqual(analysis["suggested_quality_tier"], 1)

    def test_complex_requests_suggest_the_quality_strategy(self) -> None:
        analysis = analyze_request(
            request(
                tools=[
                    {"type": "function", "function": {"name": "f", "parameters": {}}},
                ]
            )
        )
        self.assertEqual(analysis["suggested_strategy"], "highest_quality")
        self.assertEqual(analysis["suggested_quality_tier"], 5)

    def test_sensitive_content_is_flagged_without_blocking(self) -> None:
        analysis = analyze_request(
            request(
                messages=[
                    {
                        "role": "user",
                        "content": "my key is sk-abcdefghijklmnopqrstuvwx and my email is a@b.co",
                    }
                ]
            )
        )
        self.assertIn("contains_sensitive_data", analysis["flags"])
        self.assertIn("possible_credential_leak", analysis["flags"])
        # The analysis reports; it never refuses. A heuristic that silently drops
        # traffic is worse than one that surfaces the finding.
        self.assertIn("prompt_tokens", analysis)

    def test_oversized_prompt_is_flagged_against_the_limit(self) -> None:
        analysis = analyze_request(request(), max_tokens=1)
        self.assertIn("prompt_exceeds_analysis_limit", analysis["flags"])

    def test_predominantly_non_ascii_is_flagged(self) -> None:
        analysis = analyze_request(
            request(messages=[{"role": "user", "content": "こんにちは、今日の天気を教えてください。"}])
        )
        self.assertIn("predominantly_non_ascii", analysis["flags"])

    def test_completion_estimate_prefers_the_newer_field(self) -> None:
        analysis = analyze_request(request(max_tokens=100, max_completion_tokens=250))
        self.assertEqual(analysis["completion_tokens_estimate"], 250)

    def test_missing_messages_does_not_raise(self) -> None:
        analysis = analyze_request({"model": "gpt-4o"})
        self.assertEqual(analysis["message_count"], 0)
        self.assertEqual(analysis["prompt_tokens"], 0)


class TestAnalyzeText(unittest.TestCase):
    def test_bare_prompt_is_analysed(self) -> None:
        analysis = analyze_text("Explain how TCP handshakes work.")
        self.assertGreater(analysis["prompt_tokens"], 0)
        self.assertEqual(analysis["capabilities"], ["chat"])

    def test_code_prompt_is_flagged_and_escalated(self) -> None:
        analysis = analyze_text("```python\nprint('hi')\n```")
        self.assertTrue(analysis["contains_code"])
        self.assertIn("contains_code", analysis["flags"])
        self.assertEqual(analysis["complexity"], "moderate")

    def test_empty_prompt_is_handled(self) -> None:
        analysis = analyze_text("")
        self.assertEqual(analysis["prompt_tokens"], 0)


class TestScriptProfile(unittest.TestCase):
    def test_ascii_text_has_a_zero_ratio(self) -> None:
        self.assertEqual(non_ascii_ratio("plain english"), 0.0)

    def test_non_ascii_text_reports_a_ratio(self) -> None:
        self.assertGreater(non_ascii_ratio("こんにちは"), 0.9)

    def test_mixed_text_reports_a_partial_ratio(self) -> None:
        ratio = non_ascii_ratio("hello こんにちは")
        self.assertGreater(ratio, 0.0)
        self.assertLess(ratio, 1.0)

    def test_empty_text_has_a_zero_ratio(self) -> None:
        self.assertEqual(non_ascii_ratio(""), 0.0)


if __name__ == "__main__":
    unittest.main()
