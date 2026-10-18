import type { TimeBucket, UsageSummary } from './types';

/** A chart point that carries a bucket start, and its width when the API
 * reports one. */
export interface BucketLike {
  start: string;
  interval?: string;
}

/**
 * Whether an interval is measured in days or weeks rather than hours.
 *
 * Daily and weekly buckets are labelled by date: repeating "00:00" for every
 * point says nothing about where a week or a month went.
 */
function isWideInterval(interval: string): boolean {
  return interval === '1d' || interval === '1w';
}

/**
 * Infer whether points are day-bucketed from the gap between the first two.
 *
 * The cost series endpoint chooses hour, day or week truncation from the window
 * width and does not repeat the interval on every point, so the only evidence
 * is the spacing itself. Hourly points are one hour apart; anything a day or
 * more apart is a wide bucket.
 */
function inferWide(buckets: BucketLike[]): boolean {
  if (buckets.length < 2) return false;
  const first = new Date(buckets[0]?.start ?? '').getTime();
  const second = new Date(buckets[1]?.start ?? '').getTime();
  if (!Number.isFinite(first) || !Number.isFinite(second)) return false;
  return second - first >= 24 * 60 * 60 * 1000;
}

/** Reduce a bucket list into chart-ready axis labels, always in UTC.
 *
 * The buckets, the API, the audit log and the request table all speak UTC.
 * Rendering the axis in the viewer's local zone would shift every label by a
 * non-hour offset in half-hour zones (09:30 for a 04:00 bucket) and make two
 * operators in different zones see different charts for the same data. UTC
 * with an explicit note on the card is the only consistent choice.
 *
 * Points that carry no interval (the cost series) fall back to the spacing
 * between buckets. Without that, a 7-day window -- whose points are daily --
 * renders a row of identical times of day and reads like the 24-hour view.
 */
export function bucketLabels(buckets: BucketLike[]): string[] {
  const wideFallback = inferWide(buckets);
  return buckets.map((bucket) => {
    const date = new Date(bucket.start);
    // A week or month view buckets by day, where a time of day is noise; an
    // hour view needs the hour to be legible.
    const wide = bucket.interval ? isWideInterval(bucket.interval) : wideFallback;
    const options: Intl.DateTimeFormatOptions = wide
      ? { month: 'short', day: 'numeric', timeZone: 'UTC' }
      : { hour: '2-digit', minute: '2-digit', timeZone: 'UTC' };
    return date.toLocaleString(undefined, options);
  });
}

export function successRate(summary: UsageSummary | undefined): number {
  if (!summary || summary.requests === 0) return 0;
  return summary.successes / summary.requests;
}

export function outcomeErrorRate(summary: UsageSummary | undefined): number {
  if (!summary || summary.requests === 0) return 0;
  return (summary.errors + summary.rejections) / summary.requests;
}

/**
 * The proportional change from one window to the next.
 *
 * Returns null when the previous window is zero: "up 400% from nothing" is not
 * a defensible statement, and rendering it as one makes the card meaningless.
 */
export function deltaRatio(current: number | undefined, previous: number | undefined): number | null {
  if (current === undefined || previous === undefined) return null;
  if (!Number.isFinite(current) || !Number.isFinite(previous)) return null;
  if (previous === 0) return null;
  return (current - previous) / previous;
}

/** Sum a bucket field across a window. */
export function sumBuckets(buckets: TimeBucket[] | null | undefined, field: keyof TimeBucket): number {
  if (!buckets) return 0;
  return buckets.reduce((total, bucket) => {
    const value = bucket[field];
    return total + (typeof value === 'number' ? value : 0);
  }, 0);
}

/**
 * The peak value across a set of series, floored so a chart never divides by
 * zero.
 */
export function seriesMax(series: Array<{ values: number[] }>): number {
  let max = 0;
  for (const item of series) {
    for (const value of item.values) {
      if (Number.isFinite(value) && value > max) max = value;
    }
  }
  return max > 0 ? max : 1;
}
