package cost

import (
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// Evaluate turns a budget and its current spend into the dashboard's status:
// how much is left, whether the burn pace overspends the period, and what
// the meter will read at period end if nothing changes.
//
// Burn math: pace = spent/elapsedFraction, projected = spent/elapsedFraction
// scaled to the full period. A budget one hour into a month with a dollar
// spent is burning fast but has spent little — burn rate surfaces the pace
// while utilization surfaces the damage, and the dashboard needs both.
func Evaluate(b domain.Budget, spentUSD, limitUSD float64, now time.Time) domain.BudgetStatus {
	st := domain.BudgetStatus{
		TenantID: b.TenantID,
		Scope:    b.Scope,
		Period:   b.Period,
		LimitUSD: limitUSD,
		SpentUSD: RoundMicro(spentUSD),
		Enforced: b.Enforced,
		ResetAt:  b.ResetAt,
	}
	st.RemainingUSD = RoundMicro(limitUSD - spentUSD)
	if st.RemainingUSD < 0 {
		st.RemainingUSD = 0
	}
	if limitUSD > 0 {
		st.Utilization = RoundMicro(spentUSD / limitUSD)
	} else if spentUSD > 0 {
		st.Utilization = 1
	}

	elapsed, total := periodFractions(b.Period, now)
	if total > 0 && elapsed > 0 {
		pace := spentUSD / elapsed
		// Burn 1.0 spends exactly the period's allowance at the current pace:
		// pace divided by the allowance-per-day the limit implies.
		st.BurnRate = RoundMicro(pace / (limitUSD / total))
		st.ProjectedUSD = RoundMicro(pace * total)
	} else if total > 0 {
		st.ProjectedUSD = RoundMicro(spentUSD)
	}
	st.Exhausted = limitUSD > 0 && spentUSD >= limitUSD
	return st
}

// periodFractions returns (elapsedDays, totalDays) for the period containing
// now, in calendar terms: a monthly budget's period is the actual month, so
// February burn is judged against 28 days, not a nominal 30.
func periodFractions(period domain.BudgetPeriod, now time.Time) (elapsed, total float64) {
	now = now.UTC()
	switch period {
	case domain.BudgetDaily:
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		return now.Sub(start).Hours() / 24, 1
	case domain.BudgetWeekly:
		// Weeks run Monday to Monday; Go's Weekday puts Sunday at 0.
		daysSinceMonday := (int(now.Weekday()) + 6) % 7
		start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).
			AddDate(0, 0, -daysSinceMonday)
		return now.Sub(start).Hours() / 24, 7
	case domain.BudgetMonthly:
		start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
		end := start.AddDate(0, 1, 0)
		return now.Sub(start).Hours() / 24, end.Sub(start).Hours() / 24
	default: // BudgetTotal and unknown: no pace exists without an end.
		return 0, 0
	}
}

// CrossedThresholds returns the alert thresholds newly breached: every
// threshold at or below spent that is not already in fired. Comparing against
// persisted alerts (rather than recomputing from scratch) is what makes
// alerting exactly-once per period instead of once per dashboard refresh.
func CrossedThresholds(thresholds []float64, spentUSD float64, fired map[float64]bool) []float64 {
	var crossed []float64
	for _, t := range thresholds {
		if t <= 0 {
			continue
		}
		if spentUSD >= t && !fired[t] {
			crossed = append(crossed, RoundMicro(t))
		}
	}
	return crossed
}
