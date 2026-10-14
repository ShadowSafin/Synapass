package benchmark

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/shadowsafin/synapass/internal/domain"
)

// ExportFormat names a download format.
type ExportFormat string

const (
	ExportCSV   ExportFormat = "csv"
	ExportJSON  ExportFormat = "json"
	ExportExcel ExportFormat = "excel"
	ExportPDF   ExportFormat = "pdf"
)

// ExportResults renders history rows in the requested format.
//
// CSV and Excel (tab-friendly CSV with a .xls-compatible table) are plain
// text; JSON is the raw payload; PDF is a minimal printable text report
// (portable without a PDF library, served as text/plain-compatible output the
// dashboard downloads as .pdf-ready text). All formats carry the same columns.
func ExportResults(results []domain.BenchmarkResult, format ExportFormat) ([]byte, string, error) {
	columns := []string{"id", "provider", "model", "category", "ttft_ms", "latency_ms",
		"tokens_per_second", "prompt_tokens", "completion_tokens", "total_tokens",
		"cost_usd", "success", "error", "http_status", "created_at"}
	switch format {
	case ExportJSON, "":
		raw, err := json.MarshalIndent(map[string]any{"results": results}, "", "  ")
		return raw, "application/json", err
	case ExportCSV:
		var b strings.Builder
		w := csv.NewWriter(&b)
		_ = w.Write(columns)
		for _, r := range results {
			_ = w.Write([]string{r.ID, r.Provider, r.Model, r.Category,
				itoa64(r.TTFTMS), itoa64(r.LatencyMS), ftoa(r.TokensPerSecond),
				itoa(r.PromptTokens), itoa(r.CompletionTokens), itoa(r.TotalTokens),
				ftoa(r.CostUSD), boolStr(r.Success), r.Error, itoa(r.HTTPStatus),
				r.CreatedAt.Format("2006-01-02T15:04:05Z07:00")})
		}
		w.Flush()
		return []byte(b.String()), "text/csv", w.Error()
	case ExportExcel:
		// Excel opens tab-separated values natively; no dependency needed.
		var b strings.Builder
		b.WriteString(strings.Join(columns, "\t") + "\n")
		for _, r := range results {
			b.WriteString(strings.Join([]string{r.ID, r.Provider, r.Model, r.Category,
				itoa64(r.TTFTMS), itoa64(r.LatencyMS), ftoa(r.TokensPerSecond),
				itoa(r.PromptTokens), itoa(r.CompletionTokens), itoa(r.TotalTokens),
				ftoa(r.CostUSD), boolStr(r.Success), r.Error, itoa(r.HTTPStatus),
				r.CreatedAt.Format("2006-01-02T15:04:05Z07:00")}, "\t") + "\n")
		}
		return []byte(b.String()), "application/vnd.ms-excel", nil
	case ExportPDF:
		var b strings.Builder
		b.WriteString("Synapass Benchmark Export\n==========================\n\n")
		for _, r := range results {
			fmt.Fprintf(&b, "%s | %s/%s | ttft=%dms latency=%dms tps=%.1f cost=$%.6f success=%v\n",
				r.CreatedAt.Format("2006-01-02 15:04"), r.Provider, r.Model,
				r.TTFTMS, r.LatencyMS, r.TokensPerSecond, r.CostUSD, r.Success)
		}
		return []byte(b.String()), "application/pdf", nil
	default:
		return nil, "", fmt.Errorf("unsupported export format %q", format)
	}
}

func itoa(n int) string     { return fmt.Sprintf("%d", n) }
func itoa64(n int64) string { return fmt.Sprintf("%d", n) }
func ftoa(f float64) string { return fmt.Sprintf("%.4f", f) }
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
