"""Tests for the scoring primitives.

These use the standard library's ``unittest`` rather than pytest so the suite runs
on a bare interpreter: the whole point of keeping the scoring module
dependency-free is that its behaviour can be verified anywhere.
"""

from __future__ import annotations

import unittest

from synapass_workers.scoring import (
    DEFAULT_WEIGHTS,
    SCORERS,
    char_ngram_similarity,
    composite_score,
    exact_match,
    jaccard,
    length_ratio,
    mean,
    normalize_text,
    percentile,
    rank_candidates,
    resolve_metrics,
    score_candidate,
    token_f1,
    token_precision,
    token_recall,
    tokenize,
)


class TestNormalization(unittest.TestCase):
    def test_folds_case_and_collapses_whitespace(self) -> None:
        self.assertEqual(normalize_text("  Hello   World  "), "hello world")

    def test_strips_punctuation_so_numbering_style_does_not_matter(self) -> None:
        # The same amount written two ways is the same answer; a scorer that
        # disagreed would penalise formatting rather than correctness.
        self.assertEqual(normalize_text("USD 1,000.00"), "usd 1 000 00")
        self.assertEqual(normalize_text("USD 1000"), "usd 1000")

    def test_normalizes_unicode_compatibility_forms(self) -> None:
        # Full-width characters are a common copy-paste artefact from CJK input
        # methods and must not defeat comparison.
        self.assertEqual(normalize_text("ＡＢＣ"), normalize_text("ABC"))  # noqa: RUF001

    def test_empty_input_is_empty(self) -> None:
        self.assertEqual(normalize_text(""), "")
        self.assertEqual(normalize_text("   "), "")


class TestTokenization(unittest.TestCase):
    def test_splits_on_word_characters(self) -> None:
        self.assertEqual(tokenize("Hello, world!"), ["hello", "world"])

    def test_keeps_digits_and_underscores(self) -> None:
        self.assertEqual(tokenize("gpt_4o costs 12"), ["gpt_4o", "costs", "12"])


class TestExactMatch(unittest.TestCase):
    def test_identical_after_normalization(self) -> None:
        self.assertEqual(exact_match("Paris.", "paris"), 1.0)

    def test_different_answers_do_not_match(self) -> None:
        self.assertEqual(exact_match("Paris", "London"), 0.0)

    def test_both_empty_counts_as_a_match(self) -> None:
        self.assertEqual(exact_match("", ""), 1.0)


class TestTokenF1(unittest.TestCase):
    def test_identical_text_scores_one(self) -> None:
        self.assertEqual(token_f1("the quick brown fox", "the quick brown fox"), 1.0)

    def test_disjoint_text_scores_zero(self) -> None:
        self.assertEqual(token_f1("alpha beta", "gamma delta"), 0.0)

    def test_partial_overlap_is_between(self) -> None:
        score = token_f1("alpha beta gamma", "alpha beta delta")
        self.assertGreater(score, 0.0)
        self.assertLess(score, 1.0)

    def test_counts_multiplicity_so_repetition_is_penalised(self) -> None:
        once = token_f1("alpha beta", "alpha beta")
        repeated = token_f1("alpha beta", "alpha beta beta beta")
        self.assertEqual(once, 1.0)
        self.assertLess(repeated, once)

    def test_precision_and_recall_are_symmetric_for_identical_input(self) -> None:
        self.assertEqual(token_precision("a b c", "a b c"), token_recall("a b c", "a b c"))

    def test_empty_candidate_has_no_precision(self) -> None:
        self.assertEqual(token_precision("a b", ""), 0.0)

    def test_empty_reference_has_no_recall(self) -> None:
        self.assertEqual(token_recall("", "a b"), 0.0)


class TestJaccard(unittest.TestCase):
    def test_matches_token_sets_ignoring_order(self) -> None:
        # Robust to a model reordering a list, which happens constantly and is
        # rarely a correctness problem.
        self.assertEqual(jaccard("one two three", "three one two"), 1.0)

    def test_partial_overlap(self) -> None:
        self.assertAlmostEqual(jaccard("a b c", "b c d"), 2 / 4)

    def test_two_empty_sets_are_identical(self) -> None:
        self.assertEqual(jaccard("", ""), 1.0)


class TestCharacterNgrams(unittest.TestCase):
    def test_similar_spellings_score_highly(self) -> None:
        score = char_ngram_similarity("synapass", "synapasss")
        self.assertGreater(score, 0.7)

    def test_unrelated_text_scores_low(self) -> None:
        score = char_ngram_similarity("synapass", "zzzzzzzz")
        self.assertLess(score, 0.2)

    def test_whitespace_rewrapping_does_not_matter(self) -> None:
        self.assertEqual(
            char_ngram_similarity("the quick brown fox", "the  quick brown\nfox"),
            1.0,
        )

    def test_rejects_a_zero_width(self) -> None:
        with self.assertRaises(ValueError):
            char_ngram_similarity("abc", "abc", n=0)


class TestLengthRatio(unittest.TestCase):
    def test_identical_lengths_score_one(self) -> None:
        self.assertEqual(length_ratio("abcd", "efgh"), 1.0)

    def test_a_longer_candidate_is_capped_at_one(self) -> None:
        # Verbosity is not quality, so an overlong answer cannot score above one.
        self.assertEqual(length_ratio("abcd", "abcdefgh"), 1.0)

    def test_a_shorter_candidate_scores_proportionally(self) -> None:
        self.assertAlmostEqual(length_ratio("abcdefgh", "abcd"), 0.5)


class TestMetricRegistry(unittest.TestCase):
    def test_registry_contains_every_documented_metric(self) -> None:
        for name in ("exact_match", "token_f1", "jaccard", "char_ngram", "length_ratio"):
            self.assertIn(name, SCORERS)

    def test_defaults_are_used_when_no_metrics_are_requested(self) -> None:
        self.assertEqual(resolve_metrics(None), list(DEFAULT_WEIGHTS))

    def test_unknown_metric_is_rejected(self) -> None:
        # Failing loudly beats silently scoring with a subset, which would make two
        # runs of the same job produce different numbers.
        with self.assertRaises(ValueError):
            resolve_metrics(["token_f1", "vibes"])

    def test_score_candidate_returns_one_entry_per_metric(self) -> None:
        scores = score_candidate("the quick brown fox", "the quick brown fox", ["token_f1", "jaccard"])
        self.assertEqual(set(scores), {"token_f1", "jaccard"})
        self.assertTrue(all(value == 1.0 for value in scores.values()))


class TestCompositeScore(unittest.TestCase):
    def test_weighted_average_of_supplied_scores(self) -> None:
        scores = {"token_f1": 1.0, "char_ngram": 0.5}
        weights = {"token_f1": 0.5, "char_ngram": 0.5}
        self.assertAlmostEqual(composite_score(scores, weights), 0.75)

    def test_metrics_without_a_weight_do_not_contribute(self) -> None:
        # A diagnostic that nobody weighted must not silently pull the score.
        scores = {"token_f1": 1.0, "length_ratio": 0.0}
        self.assertAlmostEqual(composite_score(scores, {"token_f1": 1.0}), 1.0)

    def test_zero_total_weight_yields_zero(self) -> None:
        self.assertEqual(composite_score({"token_f1": 1.0}, {"token_f1": 0.0}), 0.0)

    def test_empty_scores_yield_zero(self) -> None:
        self.assertEqual(composite_score({}, DEFAULT_WEIGHTS), 0.0)


class TestRanking(unittest.TestCase):
    def test_orders_best_first(self) -> None:
        scored = {
            "worse": {"token_f1": 0.1, "char_ngram": 0.1, "exact_match": 0.0},
            "better": {"token_f1": 1.0, "char_ngram": 1.0, "exact_match": 1.0},
            "middle": {"token_f1": 0.5, "char_ngram": 0.5, "exact_match": 0.0},
        }
        ranking = rank_candidates(scored)
        self.assertEqual([candidate for candidate, _ in ranking], ["better", "middle", "worse"])

    def test_ties_are_broken_by_id_so_runs_are_reproducible(self) -> None:
        # A ranking that reshuffles between identical runs makes an A/B comparison
        # between two providers impossible to interpret.
        scored = {
            "zebra": {"token_f1": 0.5},
            "alpha": {"token_f1": 0.5},
        }
        weights = {"token_f1": 1.0}
        first = rank_candidates(scored, weights)
        second = rank_candidates(scored, weights)
        self.assertEqual([c for c, _ in first], ["alpha", "zebra"])
        self.assertEqual(first, second)


class TestAggregates(unittest.TestCase):
    def test_percentile_uses_nearest_rank(self) -> None:
        values = list(range(1, 101))
        self.assertEqual(percentile(values, 50), 50)
        self.assertEqual(percentile(values, 95), 95)
        self.assertEqual(percentile(values, 100), 100)

    def test_percentile_of_an_empty_sample_is_zero(self) -> None:
        self.assertEqual(percentile([], 95), 0.0)

    def test_percentile_clamps_out_of_range_requests(self) -> None:
        self.assertEqual(percentile([3, 1, 2], -5), 1)
        self.assertEqual(percentile([3, 1, 2], 500), 3)

    def test_mean_of_an_empty_sample_is_zero(self) -> None:
        self.assertEqual(mean([]), 0.0)
        self.assertEqual(mean([2, 4]), 3.0)


if __name__ == "__main__":
    unittest.main()
