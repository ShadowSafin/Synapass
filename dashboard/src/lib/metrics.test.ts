import { describe, expect, it } from 'vitest';

import {
  bucketLabels,
  deltaRatio,
  outcomeErrorRate,
  seriesMax,
  successRate,
  sumBuckets,
} from './metrics';
import type { TimeBucket, UsageSummary } from './types';

function bucket(partial: Partial<TimeBucket> = {}): TimeBucket {
  return {
    start: '2026-10-01T00:00:00Z',
    end: '2026-10-01T01:00:00Z',
    interval: '1h',
    requests: 0,
    successes: 0,
    errors: 0,
    rejections: 0,
    fallbacks: 0,
    cache_hits: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    total_tokens: 0,
    cost_usd: 0,
    latency_p50_ms: 0,
    latency_p90_ms: 0,
    latency_p95_ms: 0,
    latency_p99_ms: 0,
    ...partial,
  };
}

function summary(partial: Partial<UsageSummary> = {}): UsageSummary {
  return {
    requests: 0,
    successes: 0,
    errors: 0,
    rejections: 0,
    canceled: 0,
    fallbacks: 0,
    error_rate: 0,
    fallback_rate: 0,
    prompt_tokens: 0,
    completion_tokens: 0,
    total_tokens: 0,
    total_cost_usd: 0,
    avg_cost_usd: 0,
    avg_latency_ms: 0,
    latency_p50_ms: 0,
    latency_p90_ms: 0,
    latency_p95_ms: 0,
    latency_p99_ms: 0,
    unique_tenants: 0,
    unique_keys: 0,
    ...partial,
  };
}

describe('successRate', () => {
  it('divides successes by requests', () => {
    expect(successRate(summary({ requests: 4, successes: 3 }))).toBe(0.75);
    expect(successRate(summary({ requests: 4, successes: 4 }))).toBe(1);
  });

  it('reads an empty window as zero, not NaN', () => {
    expect(successRate(summary({ requests: 0, successes: 0 }))).toBe(0);
    expect(successRate(undefined)).toBe(0);
  });
});

describe('outcomeErrorRate', () => {
  it('combines errors and rejections', () => {
    expect(outcomeErrorRate(summary({ requests: 10, errors: 2, rejections: 1 }))).toBeCloseTo(0.3);
  });

  it('reads an empty window as zero, not NaN', () => {
    expect(outcomeErrorRate(summary({ requests: 0 }))).toBe(0);
    expect(outcomeErrorRate(undefined)).toBe(0);
  });
});

describe('deltaRatio', () => {
  it('computes the proportional change', () => {
    expect(deltaRatio(110, 100)).toBeCloseTo(0.1);
    expect(deltaRatio(50, 100)).toBeCloseTo(-0.5);
    expect(deltaRatio(100, 100)).toBe(0);
  });

  it('returns null when the previous window is empty', () => {
    // "Up 400% from nothing" is not a defensible statement.
    expect(deltaRatio(4, 0)).toBeNull();
    expect(deltaRatio(0, 0)).toBeNull();
  });

  it('returns null for missing or non-finite input', () => {
    expect(deltaRatio(undefined, 100)).toBeNull();
    expect(deltaRatio(100, undefined)).toBeNull();
    expect(deltaRatio(undefined, undefined)).toBeNull();
    expect(deltaRatio(NaN, 100)).toBeNull();
    expect(deltaRatio(100, NaN)).toBeNull();
    expect(deltaRatio(Infinity, 100)).toBeNull();
    expect(deltaRatio(100, Infinity)).toBeNull();
  });
});

describe('sumBuckets', () => {
  it('sums a field across the window', () => {
    const buckets = [bucket({ requests: 2 }), bucket({ requests: 3 })];
    expect(sumBuckets(buckets, 'requests')).toBe(5);
  });

  it('reads an empty window as zero', () => {
    expect(sumBuckets([], 'requests')).toBe(0);
    expect(sumBuckets(null, 'requests')).toBe(0);
    expect(sumBuckets(undefined, 'requests')).toBe(0);
  });

  it('ignores non-numeric fields instead of producing NaN', () => {
    const broken = bucket({ requests: 3 });
    (broken as unknown as Record<string, unknown>).requests = 'lots';
    expect(sumBuckets([broken], 'requests')).toBe(0);
  });
});

describe('seriesMax', () => {
  it('finds the peak across series', () => {
    expect(seriesMax([{ values: [1, 5, 3] }, { values: [2, 4] }])).toBe(5);
  });

  it('floors an empty or flat window at one so charts never divide by zero', () => {
    expect(seriesMax([])).toBe(1);
    expect(seriesMax([{ values: [] }])).toBe(1);
    expect(seriesMax([{ values: [0, 0] }])).toBe(1);
  });

  it('ignores non-finite values', () => {
    expect(seriesMax([{ values: [NaN, Infinity, 7] }])).toBe(7);
    expect(seriesMax([{ values: [NaN] }])).toBe(1);
  });
});

describe('bucketLabels', () => {
  it('returns no labels for an empty window', () => {
    expect(bucketLabels([])).toEqual([]);
  });

  it('renders hour buckets with a time of day in UTC', () => {
    const labels = bucketLabels([bucket({ start: '2026-10-01T04:00:00Z', interval: '1h' })]);
    expect(labels).toHaveLength(1);
    expect(labels[0]).toContain('04:00');
  });

  it('renders day and week buckets without a time of day', () => {
    const day = bucketLabels([bucket({ start: '2026-10-01T00:00:00Z', interval: '1d' })]);
    expect(day[0]).toMatch(/Oct/);
    expect(day[0]).not.toContain(':');
    const week = bucketLabels([bucket({ start: '2026-10-01T00:00:00Z', interval: '1w' })]);
    expect(week[0]).toMatch(/Oct/);
    expect(week[0]).not.toContain(':');
  });

  it('infers day buckets from spacing when the points carry no interval', () => {
    // The cost series reports only a bucket start, so the 7-day view arrives as
    // daily points with no width. These must read as dates, not as a column of
    // identical "00:00" labels that look like the 24-hour view.
    const labels = bucketLabels([
      { start: '2026-10-11T00:00:00Z' },
      { start: '2026-10-12T00:00:00Z' },
      { start: '2026-10-13T00:00:00Z' },
    ]);
    expect(labels).toHaveLength(3);
    for (const label of labels) {
      expect(label).toMatch(/Oct/);
      expect(label).not.toContain(':');
    }
    expect(new Set(labels).size).toBe(3);
  });

  it('keeps time-of-day labels for hourly points with no interval', () => {
    const labels = bucketLabels([
      { start: '2026-10-11T04:00:00Z' },
      { start: '2026-10-11T05:00:00Z' },
    ]);
    expect(labels[0]).toContain('04:00');
    expect(labels[1]).toContain('05:00');
  });

  it('keeps labels aligned with input order', () => {
    const labels = bucketLabels([
      bucket({ start: '2026-10-01T00:00:00Z', interval: '1h' }),
      bucket({ start: '2026-10-01T01:00:00Z', interval: '1h' }),
    ]);
    expect(labels).toHaveLength(2);
    expect(labels[0]).not.toBe(labels[1]);
  });
});
