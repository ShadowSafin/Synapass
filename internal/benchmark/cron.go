package benchmark

import (
	"fmt"
	"strings"
	"time"

	"github.com/shadowsafin/synapass/internal/domain"
)

// NextRunAfter computes the next scheduled run after `after` for a job.
//
// Supported schedules are the documented presets plus a custom cron
// expression. Cron supports the standard 5-field form
// (minute hour dom month dow) with "*", "*/n", lists, ranges and steps.
// Anything unparseable falls back to hourly rather than breaking the loop.
func NextRunAfter(schedule, cronExpr string, after time.Time) time.Time {
	if schedule == "" {
		schedule = domain.BenchmarkEveryHour
	}
	if intervals := domain.BenchmarkScheduleIntervals(); schedule != domain.BenchmarkCustomCron {
		if d, ok := intervals[schedule]; ok && d > 0 {
			return after.Truncate(d).Add(d)
		}
		return after.Add(time.Hour)
	}
	if next, ok := nextCronRun(cronExpr, after); ok {
		return next
	}
	return after.Add(time.Hour)
}

// ValidSchedule reports whether a schedule/cron pair is acceptable.
func ValidSchedule(schedule, cronExpr string) error {
	if schedule == "" {
		return nil
	}
	switch schedule {
	case domain.BenchmarkEveryMinute, domain.BenchmarkEvery5Minutes,
		domain.BenchmarkEvery15Min, domain.BenchmarkEvery30Min,
		domain.BenchmarkEveryHour, domain.BenchmarkEvery3Hours,
		domain.BenchmarkEvery6Hours, domain.BenchmarkEvery12Hours,
		domain.BenchmarkEvery24Hours:
		return nil
	case domain.BenchmarkCustomCron:
		if strings.TrimSpace(cronExpr) == "" {
			return fmt.Errorf("cron_expression is required when schedule is custom")
		}
		if _, ok := nextCronRun(cronExpr, time.Now().UTC()); !ok {
			return fmt.Errorf("cron_expression %q is not a supported 5-field cron", cronExpr)
		}
		return nil
	default:
		return fmt.Errorf("schedule %q is not one of the supported presets or custom", schedule)
	}
}

// nextCronRun finds the next minute-boundary matching a 5-field cron.
// It scans forward at most ~366 days; the bound keeps a pathological
// expression from looping forever.
func nextCronRun(expr string, after time.Time) (time.Time, bool) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return time.Time{}, false
	}
	minutes, ok1 := parseCronField(fields[0], 0, 59)
	hours, ok2 := parseCronField(fields[1], 0, 23)
	doms, ok3 := parseCronField(fields[2], 1, 31)
	months, ok4 := parseCronField(fields[3], 1, 12)
	dows, ok5 := parseCronField(fields[4], 0, 6)
	if !(ok1 && ok2 && ok3 && ok4 && ok5) {
		return time.Time{}, false
	}
	candidate := after.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		dow := int(candidate.Weekday())
		if minutes[candidate.Minute()] && hours[candidate.Hour()] &&
			doms[candidate.Day()] && months[int(candidate.Month())] && dows[dow] {
			return candidate, true
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, false
}

func parseCronField(field string, min, max int) (map[int]bool, bool) {
	out := map[int]bool{}
	if field == "*" {
		for i := min; i <= max; i++ {
			out[i] = true
		}
		return out, true
	}
	for _, part := range strings.Split(field, ",") {
		step := 1
		base := part
		if idx := strings.Index(part, "/"); idx >= 0 {
			base = part[:idx]
			var n int
			if _, err := fmt.Sscanf(part[idx+1:], "%d", &n); err != nil || n <= 0 {
				return nil, false
			}
			step = n
		}
		lo, hi := min, max
		switch {
		case base == "" || base == "*":
		case strings.Contains(base, "-"):
			var a, b int
			if _, err := fmt.Sscanf(base, "%d-%d", &a, &b); err != nil || a < min || b > max || a > b {
				return nil, false
			}
			lo, hi = a, b
		default:
			var v int
			if _, err := fmt.Sscanf(base, "%d", &v); err != nil || v < min || v > max {
				return nil, false
			}
			lo, hi = v, v
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	return out, len(out) > 0
}
