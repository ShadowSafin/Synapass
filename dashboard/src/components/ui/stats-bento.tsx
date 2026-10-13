'use client';

import * as React from 'react';

import { cn } from '@/lib/utils';

export interface BentoPrimary {
  eyebrow: string;
  value: string;
  caption: string;
}

export interface BentoTrend {
  label: string;
  value: string;
  /** Per-bucket values rendered as bars, normalized to the tallest. */
  spark: number[];
}

export interface BentoMini {
  value: string;
  label: string;
  icon?: React.ComponentType<{ className?: string }>;
}

export interface StatsBentoProps {
  primary: BentoPrimary;
  trend: BentoTrend;
  miniA: BentoMini;
  miniB: BentoMini;
  className?: string;
}

/**
 * A bento summary: one primary figure plus three supporting cells.
 *
 * Every number is passed in — the component renders, it never invents. The
 * layout collapses to a single column on narrow screens.
 */
export function StatsBento({ primary, trend, miniA, miniB, className }: StatsBentoProps) {
  const peak = Math.max(...trend.spark, 1);
  const MiniIcon = miniB.icon;

  return (
    <section className={cn('grid min-w-0 grid-cols-1 gap-4 md:grid-cols-6', className)} aria-label="Key figures">
      {/* Primary figure. Flat primary surface, no texture: the number is the decoration. */}
      <div className="flex min-w-0 flex-col justify-between overflow-hidden rounded-2xl bg-primary p-4 sm:p-8 md:col-span-3">
        <div className="min-w-0">
          <span className="mb-4 inline-block rounded-full bg-primary-foreground/10 px-3 py-1 text-[10px] font-semibold uppercase tracking-[0.08em] text-primary-foreground/70">
            {primary.eyebrow}
          </span>
          <p className="break-words text-4xl font-semibold tracking-tight tabular-nums text-primary-foreground sm:text-6xl">
            {primary.value}
          </p>
        </div>
        <p className="mt-6 max-w-xs text-sm text-primary-foreground/70">{primary.caption}</p>
      </div>

      {/* Trend with a sparkline of real per-bucket values. */}
      <div className="flex min-w-0 items-center justify-between gap-4 rounded-2xl border border-border bg-card p-4 sm:p-8 md:col-span-3">
        <div className="min-w-0">
          <p className="mb-1 text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
            {trend.label}
          </p>
          <p className="break-words text-2xl font-semibold tabular-nums text-foreground sm:text-3xl">{trend.value}</p>
        </div>
        <div
          className="flex h-10 shrink-0 items-end gap-1"
          role="img"
          aria-label={`${trend.label} trend: ${trend.value}`}
        >
          {trend.spark.map((entry, index) => (
            <div
              key={index}
              className="w-1.5 rounded-full bg-primary"
              style={{ height: `${Math.max(8, (entry / peak) * 100)}%` }}
            />
          ))}
        </div>
      </div>

      {/* Supporting figures. */}
      <div className="flex min-w-0 flex-col justify-center rounded-2xl border border-border bg-card p-4 text-center sm:p-6 md:col-span-2">
        <p className="break-words text-2xl font-semibold tabular-nums text-foreground">{miniA.value}</p>
        <p className="mt-1 text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">
          {miniA.label}
        </p>
      </div>

      <div className="flex min-w-0 items-center gap-4 rounded-2xl border border-border bg-card p-4 sm:p-6 md:col-span-4">
        <div className="flex size-10 shrink-0 items-center justify-center rounded-full bg-muted text-foreground">
          {MiniIcon ? <MiniIcon className="size-5" /> : null}
        </div>
        <div>
          <p className="text-sm font-semibold leading-none tabular-nums text-foreground">{miniB.value}</p>
          <p className="mt-1 text-xs text-muted-foreground">{miniB.label}</p>
        </div>
      </div>
    </section>
  );
}

export default StatsBento;
