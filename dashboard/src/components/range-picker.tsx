'use client';

import Link from 'next/link';
import { usePathname, useSearchParams } from 'next/navigation';
import * as React from 'react';

import { cn } from '@/lib/utils';

/** The windows offered. Each maps to the `window` query parameter the API accepts. */
export const WINDOWS = [
  { value: '1h', label: '1h' },
  { value: '6h', label: '6h' },
  { value: '24h', label: '24h' },
  { value: '7d', label: '7d' },
  { value: '30d', label: '30d' },
] as const;

export type WindowValue = (typeof WINDOWS)[number]['value'];

export const DEFAULT_WINDOW: WindowValue = '24h';

/** Read the current window from the URL, falling back to the default. */
export function useWindowParam(): WindowValue {
  const params = useSearchParams();
  const raw = params.get('window');
  const match = WINDOWS.find((item) => item.value === raw);
  return match?.value ?? DEFAULT_WINDOW;
}

/**
 * A segmented control that writes the window into the URL.
 *
 * Keeping the range in the query string rather than component state means a link
 * to a view is shareable and a browser refresh preserves it -- which is what an
 * operator sending an incident link expects.
 */
export function RangePicker({ value }: { value: WindowValue }) {
  const pathname = usePathname();
  const params = useSearchParams();

  const hrefFor = (next: string): string => {
    const search = new URLSearchParams(params.toString());
    search.set('window', next);
    return `${pathname}?${search.toString()}`;
  };

  return (
    <div
      role="group"
      aria-label="Time window"
      className="inline-flex max-w-full items-center overflow-x-auto rounded-xl border border-white/[0.08] bg-[#121217] p-1 shadow-sm [-webkit-overflow-scrolling:touch]"
    >
      {WINDOWS.map((item) => (
        <Link
          key={item.value}
          href={hrefFor(item.value)}
          aria-current={item.value === value ? 'true' : undefined}
          className={cn(
            'shrink-0 rounded-lg px-2.5 py-1 text-xs font-medium transition-all',
            item.value === value
              ? 'bg-white/[0.1] text-white shadow-sm ring-1 ring-white/[0.08]'
              : 'text-neutral-400 hover:bg-white/[0.04] hover:text-white',
          )}
        >
          {item.label}
        </Link>
      ))}
    </div>
  );
}

/**
 * Convert a window label into the `from` query value the API expects.
 *
 * The gateway parses `from` with Go's time.ParseDuration, which has no day
 * unit: sending "7d" would be rejected. Days are expressed as hours instead.
 */
export function windowToFromParam(window: WindowValue): string {
  switch (window) {
    case '1h':
      return '1h';
    case '6h':
      return '6h';
    case '24h':
      return '24h';
    case '7d':
      return '168h';
    case '30d':
      return '720h';
    default:
      return '24h';
  }
}
