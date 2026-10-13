'use client';

import * as React from 'react';

import { cn } from '@/lib/utils';

export interface BarRow {
  label: string;
  value: number;
  /** Optional right-aligned secondary text, e.g. a cost. */
  detail?: string;
  /** Renders the row in a warning or danger tone, e.g. an unhealthy provider. */
  tone?: 'default' | 'warning' | 'danger';
}

const TONE_CLASS: Record<NonNullable<BarRow['tone']>, string> = {
  default: 'bg-gradient-to-r from-blue-600 to-sky-400',
  warning: 'bg-gradient-to-r from-amber-600 to-amber-400',
  danger: 'bg-gradient-to-r from-rose-600 to-rose-400',
};

/**
 * A ranked horizontal bar list.
 *
 * A bar list is preferred to a pie chart for these breakdowns: the categories
 * are ranked, the values are of wildly different magnitude, and reading a
 * percentage off a wedge is harder than reading it off a bar.
 */
export function BarList({
  rows,
  formatValue = (value) => String(value),
  emptyMessage = 'No data',
  className,
}: {
  rows: BarRow[];
  formatValue?: (value: number) => string;
  emptyMessage?: string;
  className?: string;
}) {
  if (rows.length === 0) {
    return <p className={cn('py-6 text-center text-xs text-neutral-400', className)}>{emptyMessage}</p>;
  }

  const max = Math.max(...rows.map((row) => row.value), 1);

  return (
    <ul className={cn('space-y-3', className)}>
      {rows.map((row) => {
        const width = Math.max(2, (row.value / max) * 100);
        return (
          <li key={row.label} className="min-w-0 space-y-1.5">
            <div className="flex min-w-0 items-baseline justify-between gap-2 text-xs">
              <span className="min-w-0 flex-1 truncate font-medium text-neutral-200" title={row.label}>
                {row.label}
              </span>
              <span className="shrink-0 tabular-nums text-neutral-400">
                {formatValue(row.value)}
                {row.detail ? <span className="ml-2 text-neutral-400">{row.detail}</span> : null}
              </span>
            </div>
            <div className="h-1.5 w-full overflow-hidden rounded-full bg-white/[0.06]">
              <div
                className={cn('h-full rounded-full transition-all duration-300', TONE_CLASS[row.tone ?? 'default'])}
                style={{ width: `${width}%` }}
              />
            </div>
          </li>
        );
      })}
    </ul>
  );
}
