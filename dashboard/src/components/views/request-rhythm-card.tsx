'use client';

import * as React from 'react';

import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { ActivityHeatmap, type ActivityDay } from '@/components/ui/activity-heatmap';
import { ChartSkeleton, ErrorState } from '@/components/ui/state';
import { useAnalyticsReport } from '@/hooks/use-admin';
import type { TimeBucket } from '@/lib/types';

const RANGE_OPTIONS = [
  { hours: 4, label: '4H', period: 'the last 4 hours' },
  { hours: 24, label: '24H', period: 'the last 24 hours' },
  { hours: 168, label: '7D', period: 'the last 7 days' },
] as const;

const METRIC_OPTIONS = [
  { value: 'requests', label: 'Requests' },
  { value: 'errors', label: 'Errors' },
] as const;

type Metric = (typeof METRIC_OPTIONS)[number]['value'];

/** Fixed grid extent: all three ranges render the same trailing 7-day grid. */
const GRID_DAYS = 7;

const DAY_MS = 86_400_000;

/**
 * Collapse analytics buckets (hourly or daily, depending on the window) into
 * one count per UTC calendar day over the fixed trailing 7-day grid. Only days
 * inside the selected window keep their counts; the rest render as empty cells,
 * so 4H looks the same size as a fully populated 7D.
 */
function bucketsToDays(buckets: TimeBucket[], metric: Metric, windowDays: number): ActivityDay[] {
  const perDay = new Map<string, number>();
  for (const bucket of buckets) {
    const time = Date.parse(bucket.start);
    if (!Number.isFinite(time)) continue;
    const date = new Date(time).toISOString().slice(0, 10);
    const value = metric === 'errors' ? bucket.errors : bucket.requests;
    if (typeof value === 'number' && Number.isFinite(value) && value > 0) {
      perDay.set(date, (perDay.get(date) ?? 0) + value);
    }
  }
  const today = new Date();
  today.setUTCHours(0, 0, 0, 0);
  const cutoff = new Date(today.getTime() - (windowDays - 1) * DAY_MS).toISOString().slice(0, 10);
  const out: ActivityDay[] = [];
  for (let back = GRID_DAYS - 1; back >= 0; back--) {
    const date = new Date(today.getTime() - back * DAY_MS).toISOString().slice(0, 10);
    out.push({ date, count: date < cutoff ? 0 : (perDay.get(date) ?? 0) });
  }
  return out;
}

function Segmented<T extends string | number>({
  value,
  options,
  onChange,
  label,
}: {
  value: T;
  options: ReadonlyArray<{ value: T; label: string }>;
  onChange: (next: T) => void;
  label: string;
}) {
  return (
    <span className="inline-flex items-center rounded-lg border border-white/[0.08] bg-white/[0.02] p-0.5" role="group" aria-label={label}>
      {options.map((option) => (
        <button
          key={String(option.value)}
          type="button"
          onClick={() => onChange(option.value)}
          aria-pressed={value === option.value}
          className={
            value === option.value
              ? 'rounded-md bg-white/[0.1] px-2 py-0.5 text-[11px] font-medium text-white'
              : 'rounded-md px-2 py-0.5 text-[11px] text-neutral-400 transition-colors hover:text-white'
          }
        >
          {option.label}
        </button>
      ))}
    </span>
  );
}

/**
 * Daily request rhythm as a GitHub-style contribution calendar.
 *
 * The grid always spans the full trailing year: every day renders, quiet days
 * as empty cells and busy days filled. Sits on the Analytics page under the
 * traffic chart: the chart answers "how much", this answers "on which days".
 */
export function RequestRhythmCard({ tenantId }: { tenantId: string }) {
  const [hours, setHours] = React.useState<number>(24);
  const [metric, setMetric] = React.useState<Metric>('requests');
  const [selected, setSelected] = React.useState<string | null>(null);

  // The range switch picks which window populates the calendar. Buckets come
  // back hourly or daily depending on span; they are re-aggregated to days
  // below, so the interval does not matter.
  const { data, isPending, isError, error, refetch } = useAnalyticsReport({
    tenantId,
    from: `${hours}h`,
    top: 1,
    sort: 'requests',
  });

  // Calendar span of the selected window in whole UTC days, so a 24h window
  // crossing midnight UTC populates both days instead of just one.
  const windowDays = React.useMemo(() => {
    const now = Date.now();
    const windowStart = new Date(now - hours * 3_600_000);
    windowStart.setUTCHours(0, 0, 0, 0);
    const today = new Date(now);
    today.setUTCHours(0, 0, 0, 0);
    return Math.max(1, Math.round((today.getTime() - windowStart.getTime()) / DAY_MS) + 1);
  }, [hours]);
  // Fixed 7-day grid for every range; only the selected window is populated.
  const days = React.useMemo(
    () => bucketsToDays(data?.buckets ?? [], metric, windowDays),
    [data?.buckets, metric, windowDays],
  );
  const unit = metric === 'errors' ? { one: 'error', other: 'errors' } : { one: 'request', other: 'requests' };
  const period = RANGE_OPTIONS.find((option) => option.hours === hours)?.period ?? `the last ${hours} hours`;

  return (
    <Card>
      <CardHeader className="pb-2">
        <CardTitle className="text-base">Daily rhythm</CardTitle>
        <CardDescription>
          {metric === 'errors'
            ? 'Days with failing requests. Hover or focus a cell for the count; arrow keys move by day and week.'
            : 'Request volume per day. Streaks and quiet days read at a glance; use the traffic chart above for exact comparisons.'}
        </CardDescription>
      </CardHeader>
      <CardContent className="pt-2">
        {isPending ? (
          <ChartSkeleton height={220} />
        ) : isError ? (
          <ErrorState error={error} onRetry={() => void refetch()} />
        ) : (
          // The full grid always renders: quiet days show as empty cells and
          // populated days fill in, so the shape of the range is stable while
          // data arrives.
          <ActivityHeatmap
            days={days}
            label={`Daily ${metric} in ${period}`}
            period={period}
            unit={unit}
            selectedDate={selected}
            onSelectDate={setSelected}
            actions={
              <>
                <Segmented
                  label="Metric"
                  value={metric}
                  onChange={(next) => {
                    setMetric(next);
                    setSelected(null);
                  }}
                  options={METRIC_OPTIONS.map((o) => ({ value: o.value as Metric, label: o.label }))}
                />
                <Segmented
                  label="Range"
                  value={hours}
                  onChange={(next) => {
                    setHours(next);
                    setSelected(null);
                  }}
                  options={RANGE_OPTIONS.map((o) => ({ value: o.hours, label: o.label }))}
                />
              </>
            }
          />
        )}
      </CardContent>
    </Card>
  );
}
