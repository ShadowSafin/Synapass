import * as React from 'react';

import { cn } from '@/lib/utils';

/**
 * The header every page starts with.
 *
 * A page states what it shows and, when the answer is not obvious, what the
 * numbers mean. The `note` slot is for that second sentence.
 */
export function PageHeader({
  title,
  description,
  actions,
  note,
  className,
}: {
  title: string;
  description?: string;
  actions?: React.ReactNode;
  note?: React.ReactNode;
  className?: string;
}) {
  return (
    <header className={cn('mb-5 flex flex-col gap-4 sm:mb-6 sm:flex-row sm:items-start sm:justify-between', className)}>
      <div className="min-w-0 space-y-1">
        <h1 className="text-lg font-semibold tracking-tight text-white sm:text-xl">{title}</h1>
        {description ? <p className="max-w-3xl text-xs leading-relaxed text-neutral-400 break-words">{description}</p> : null}
        {note ? (
          <div className="mt-2 inline-flex items-center gap-1.5 rounded-lg border border-white/[0.06] bg-white/[0.02] px-2.5 py-1 text-[11px] text-neutral-400">
            {note}
          </div>
        ) : null}
      </div>
      {actions ? <div className="flex w-full flex-wrap items-center gap-2 sm:w-auto sm:shrink-0 sm:gap-2.5">{actions}</div> : null}
    </header>
  );
}
