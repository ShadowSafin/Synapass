package benchmark

import (
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// DefaultDatasetName is the seeded global dataset.
const DefaultDatasetName = "synapass-default"

// DefaultPrompts is the built-in benchmark corpus: one prompt per category so
// a fresh install produces a meaningful comparison on its first run.
func DefaultPrompts() []domain.BenchmarkPrompt {
	now := time.Now().UTC()
	mk := func(prompt string, cat domain.BenchmarkPromptCategory, diff domain.BenchmarkPromptDifficulty, maxTokens int) domain.BenchmarkPrompt {
		return domain.BenchmarkPrompt{
			ID: domain.NewID(), Prompt: prompt, Category: cat,
			Difficulty: diff, Weight: 1, MaxTokens: maxTokens, CreatedAt: now,
		}
	}
	return []domain.BenchmarkPrompt{
		mk("Write a Go function that reverses a string, handling Unicode correctly. Return only the code.", domain.BenchmarkCatCoding, domain.BenchmarkDifficultyMedium, 512),
		mk("If all Bloops are Razzies and all Razzies are Lazzies, are all Bloops Lazzies? Explain step by step.", domain.BenchmarkCatReasoning, domain.BenchmarkDifficultyEasy, 512),
		mk("Solve: a train travels 120 km at 80 km/h, then 80 km at 100 km/h. What is the average speed? Show the work.", domain.BenchmarkCatMath, domain.BenchmarkDifficultyMedium, 512),
		mk("Translate to French: 'The quick brown fox jumps over the lazy dog.'", domain.BenchmarkCatTranslation, domain.BenchmarkDifficultyEasy, 256),
		mk("Describe what you would look for in an image to determine whether it was taken at sunrise or sunset.", domain.BenchmarkCatVision, domain.BenchmarkDifficultyMedium, 512),
		mk(`Return exactly this JSON shape with real values: {"name": string, "age": number, "tags": string[]}.`, domain.BenchmarkCatJSONMode, domain.BenchmarkDifficultyEasy, 256),
		mk("Write the opening paragraph of a science-fiction story set on a tidally locked planet.", domain.BenchmarkCatCreative, domain.BenchmarkDifficultyMedium, 512),
		mk("Summarize the following in one sentence: Synapass routes inference across providers using policies, budgets and health signals.", domain.BenchmarkCatLongContext, domain.BenchmarkDifficultyEasy, 256),
		mk("You have tools: get_weather(city) and convert_temp(c, unit). The user asks for the weather in Paris in Fahrenheit. Describe the exact tool calls you would make, in order.", domain.BenchmarkCatToolCalling, domain.BenchmarkDifficultyMedium, 512),
		mk("Call function search_docs(query: string, top_k: integer) to answer: what does the routing policy do? Describe the call and the expected result handling.", domain.BenchmarkCatFunctionCalling, domain.BenchmarkDifficultyHard, 512),
	}
}
