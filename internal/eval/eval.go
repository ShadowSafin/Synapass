// Package eval implements offline evaluation: comparing provider outputs,
// detecting regressions, and scoring route quality.
package eval

import (
	"sort"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// GoldenCase is one deterministic test.
type GoldenCase struct {
	ID        string `json:"id"`
	Dataset   string `json:"dataset,omitempty"`
	Prompt    string `json:"prompt"`
	Reference string `json:"reference"`
	// MinScore is the passing threshold for the composite score.
	MinScore float64 `json:"min_score,omitempty"`
}

// CompareInput is one replayed request scored across providers.
type CompareInput struct {
	RequestID string
	Reference string
	Outputs   []ProviderOutput
	Metrics   []string
}

// ProviderOutput is one provider's answer.
type ProviderOutput struct {
	Provider  string
	Model     string
	Output    string
	LatencyMS int64
	CostUSD   float64
	Error     string
}

// CompareResult ranks outputs and flags regressions.
type CompareResult struct {
	RequestID string                        `json:"request_id"`
	Ranking   []RankedOutput                `json:"ranking"`
	Scores    map[string]map[string]float64 `json:"scores"`
	Winner    string                        `json:"winner"`
	Regressed []string                      `json:"regressed,omitempty"`
	Notes     []string                      `json:"notes,omitempty"`
}

// RankedOutput is one ranked candidate.
type RankedOutput struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// Engine scores comparisons deterministically.
type Engine struct{}

// New creates an engine.
func New() *Engine { return &Engine{} }

// Score deterministically scores one candidate answer against a reference,
// returning the composite score and the per-metric breakdown. It is the
// single-call equivalent of Compare for one output, and the function the
// replay runner wires to so stored scores never come from a second,
// divergent scorer.
func Score(reference, candidate string) (float64, map[string]float64) {
	m := scoreText(reference, candidate)
	return composite(m), m
}

// Composite combines per-metric scores into one number with the engine's
// canonical weights.
func Composite(m map[string]float64) float64 { return composite(m) }

// Compare scores every output against the reference.
func (e *Engine) Compare(in CompareInput) CompareResult {
	scores := map[string]map[string]float64{}
	for _, o := range in.Outputs {
		id := o.Provider + "/" + o.Model
		if o.Error != "" {
			continue
		}
		scores[id] = scoreText(in.Reference, o.Output)
	}
	type kv struct {
		k string
		v float64
	}
	ranked := []kv{}
	for id, m := range scores {
		ranked = append(ranked, kv{id, composite(m)})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].v != ranked[j].v {
			return ranked[i].v > ranked[j].v
		}
		return ranked[i].k < ranked[j].k
	})
	out := CompareResult{RequestID: in.RequestID, Scores: scores}
	for _, r := range ranked {
		out.Ranking = append(out.Ranking, RankedOutput{ID: r.k, Score: r.v})
	}
	if len(out.Ranking) > 0 {
		out.Winner = out.Ranking[0].ID
	}
	return out
}

// CheckRegression compares a new score against a baseline.
func (e *Engine) CheckRegression(baseline, current float64, tolerance float64) bool {
	if tolerance <= 0 {
		tolerance = 0.05
	}
	return current+tolerance < baseline
}

// RunGoldens scores golden cases for one provider output set.
func (e *Engine) RunGoldens(cases []GoldenCase, outputs map[string]string) []domain.EvaluationResult {
	out := []domain.EvaluationResult{}
	for _, c := range cases {
		got := outputs[c.ID]
		m := scoreText(c.Reference, got)
		s := composite(m)
		min := c.MinScore
		if min == 0 {
			min = 0.5
		}
		out = append(out, domain.EvaluationResult{
			ID: domain.NewID(), EvaluationID: c.Dataset,
			RequestID: c.ID, Provider: "golden", Model: c.Dataset,
			Score: s, OutputPreview: truncate(got, 500),
			IsRegression: s < min, Metrics: m, CreatedAt: time.Now().UTC(),
		})
	}
	return out
}

// scoreText computes deterministic similarity metrics.
func scoreText(reference, candidate string) map[string]float64 {
	nr, nc := normalize(reference), normalize(candidate)
	m := map[string]float64{}
	if nr == nc {
		m["exact_match"] = 1
	} else {
		m["exact_match"] = 0
	}
	m["token_f1"] = tokenF1(nr, nc)
	m["jaccard"] = jaccard(nr, nc)
	m["char_ngram"] = charTrigram(nr, nc)
	return m
}

func composite(m map[string]float64) float64 {
	return 0.6*m["token_f1"] + 0.3*m["char_ngram"] + 0.1*m["exact_match"]
}

func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func tokens(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Fields(s)
}

func tokenF1(ref, cand string) float64 {
	rt, ct := tokens(ref), tokens(cand)
	if len(ct) == 0 || len(rt) == 0 {
		if len(rt) == 0 && len(ct) == 0 {
			return 1
		}
		return 0
	}
	counts := map[string]int{}
	for _, t := range strings.Fields(ref) {
		counts[t]++
	}
	hit := 0
	for _, t := range strings.Fields(cand) {
		if counts[t] > 0 {
			counts[t]--
			hit++
		}
	}
	prec := float64(hit) / float64(len(ct))
	counts2 := map[string]int{}
	for _, t := range strings.Fields(cand) {
		counts2[t]++
	}
	hit2 := 0
	for _, t := range strings.Fields(ref) {
		if counts2[t] > 0 {
			counts2[t]--
			hit2++
		}
	}
	rec := float64(hit2) / float64(len(rt))
	if prec+rec == 0 {
		return 0
	}
	return 2 * prec * rec / (prec + rec)
}

func jaccard(ref, cand string) float64 {
	a := map[string]struct{}{}
	for _, t := range tokens(ref) {
		a[t] = struct{}{}
	}
	b := map[string]struct{}{}
	for _, t := range tokens(cand) {
		b[t] = struct{}{}
	}
	if len(a) == 0 && len(b) == 0 {
		return 1
	}
	inter := 0
	for k := range a {
		if _, ok := b[k]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func charTrigram(ref, cand string) float64 {
	ra := ngrams(strings.ReplaceAll(ref, " ", ""), 3)
	ca := ngrams(strings.ReplaceAll(cand, " ", ""), 3)
	if len(ra) == 0 && len(ca) == 0 {
		return 1
	}
	if len(ra) == 0 || len(ca) == 0 {
		return 0
	}
	dot := 0.0
	na := 0.0
	nb := 0.0
	for k, va := range ra {
		na += va * va
		if vb, ok := ca[k]; ok {
			dot += va * vb
		}
	}
	for _, vb := range ca {
		nb += vb * vb
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt(na) * sqrt(nb))
}

func ngrams(s string, n int) map[string]float64 {
	m := map[string]float64{}
	if s == "" {
		return m
	}
	if len(s) < n {
		m[s]++
		return m
	}
	for i := 0; i+n <= len(s); i++ {
		m[s[i:i+n]]++
	}
	return m
}

func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 20; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
