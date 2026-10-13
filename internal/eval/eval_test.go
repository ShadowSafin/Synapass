package eval

import "testing"

func TestCompareRanksBestFirst(t *testing.T) {
	e := New()
	res := e.Compare(CompareInput{
		RequestID: "r1",
		Reference: "the quick brown fox",
		Outputs: []ProviderOutput{
			{Provider: "a", Model: "m", Output: "the quick brown fox"},
			{Provider: "b", Model: "m", Output: "something entirely different here"},
		},
	})
	if res.Winner != "a/m" {
		t.Fatalf("expected a/m winner, got %s", res.Winner)
	}
	if len(res.Ranking) != 2 || res.Ranking[0].Score < res.Ranking[1].Score {
		t.Fatalf("ranking not descending: %+v", res.Ranking)
	}
}

func TestRegression(t *testing.T) {
	e := New()
	if !e.CheckRegression(0.9, 0.5, 0.05) {
		t.Fatalf("expected regression")
	}
	if e.CheckRegression(0.9, 0.88, 0.05) {
		t.Fatalf("unexpected regression")
	}
}

func TestGoldenThreshold(t *testing.T) {
	e := New()
	cases := []GoldenCase{{ID: "c1", Reference: "hello world", MinScore: 0.9}}
	results := e.RunGoldens(cases, map[string]string{"c1": "completely unrelated answer xyz"})
	if len(results) != 1 || !results[0].IsRegression {
		t.Fatalf("expected regression flag: %+v", results)
	}
}

func TestScoreIdenticalIsOne(t *testing.T) {
	score, metrics := Score("hello world", "hello world")
	// The trigram cosine goes through a Newton sqrt, so allow epsilon.
	if diff := 1 - score; diff < 0 || diff > 1e-9 {
		t.Fatalf("score = %v, want ~1", score)
	}
	if metrics["exact_match"] != 1 || metrics["token_f1"] != 1 || metrics["jaccard"] != 1 {
		t.Errorf("exact/token/jaccard metrics = %v, want all 1", metrics)
	}
	if diff := 1 - metrics["char_ngram"]; diff < 0 || diff > 1e-9 {
		t.Errorf("char_ngram = %v, want ~1", metrics["char_ngram"])
	}
}

func TestScoreEmptyReferenceYieldsZeroWithMetrics(t *testing.T) {
	score, metrics := Score("", "hello")
	if score != 0 {
		t.Fatalf("score = %v, want 0", score)
	}
	// The metrics must still be populated: a computed zero is evidence the
	// scorer ran, unlike a defaulted zero.
	for _, k := range []string{"exact_match", "token_f1", "jaccard", "char_ngram"} {
		v, ok := metrics[k]
		if !ok {
			t.Errorf("metric %q is missing", k)
			continue
		}
		if v != 0 {
			t.Errorf("metric %q = %v, want 0", k, v)
		}
	}
}

func TestScorePartialMatchBetweenZeroAndOne(t *testing.T) {
	score, metrics := Score("the quick brown fox", "the quick brown dog")
	if score <= 0 || score >= 1 {
		t.Fatalf("score = %v, want strictly between 0 and 1", score)
	}
	if metrics["token_f1"] <= 0 || metrics["token_f1"] >= 1 {
		t.Errorf("token_f1 = %v, want strictly between 0 and 1", metrics["token_f1"])
	}
	if got := Composite(metrics); got != score {
		t.Errorf("Composite(metrics) = %v, want Score %v", got, score)
	}
}
