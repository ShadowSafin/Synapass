'use client';

import { AlertTriangle, Inbox, KeyRound, RefreshCw, ServerCrash, WifiOff } from 'lucide-react';
import * as React from 'react';

import { ApiError } from '@/lib/api';
import { cn } from '@/lib/utils';

import { Button } from './button';
import { DotPattern } from './dot-pattern';
import { Skeleton } from './skeleton';

/**
 * The shared error surface.
 *
 * The distinctions here are deliberate: a configuration error (no admin key),
 * an authorization failure and an unreachable gateway all need different
 * actions from the operator, and collapsing them into "something went wrong"
 * is what makes a dashboard useless during the one moment it matters.
 */
export function ErrorState({
  error,
  onRetry,
  className,
}: {
  error: unknown;
  onRetry?: () => void;
  className?: string;
}) {
  const apiError = error instanceof ApiError ? error : undefined;
  const message = apiError?.message ?? (error instanceof Error ? error.message : 'An unexpected error occurred.');

  let icon = <AlertTriangle className="size-5 text-danger" />;
  let title = 'Something went wrong';
  let hint: string | undefined;

  if (apiError?.isConfigurationError) {
    icon = <KeyRound className="size-5 text-warning" />;
    title = 'The dashboard is not connected';
    hint = 'Set SYNAPASS_ADMIN_KEY to the admin key the gateway accepts, then restart it.';
  } else if (apiError?.isUnauthorized) {
    icon = <KeyRound className="size-5 text-warning" />;
    title = 'Not authorized';
    hint = 'The configured credential is missing the scope this page requires, or it has been revoked.';
  } else if (apiError?.code === 'dashboard_offline') {
    icon = <WifiOff className="size-5 text-danger" />;
    title = 'The dashboard server is unreachable';
    hint = 'The page could not reach its own API route. Check the container or dev server.';
  } else if (apiError?.status === 502) {
    icon = <ServerCrash className="size-5 text-danger" />;
    title = 'The gateway is unreachable';
    hint = 'Check that the gateway is running and that SYNAPASS_API_URL points at it.';
  } else if (apiError && apiError.status >= 500) {
    title = 'The gateway reported a failure';
  }

  return (
    <div
      role="alert"
      className={cn(
        'flex flex-col items-start gap-3 rounded-2xl border border-white/[0.08] bg-[#141419]/90 p-5 text-sm sm:p-6 shadow-sm',
        className,
      )}
    >
      <div className="flex items-center gap-2 font-medium text-white">
        {icon}
        <span>{title}</span>
      </div>
      <p className="text-xs text-neutral-300">{message}</p>
      {hint ? <p className="text-xs text-neutral-400">{hint}</p> : null}
      {onRetry ? (
        <Button variant="outline" size="sm" onClick={onRetry}>
          <RefreshCw className="size-3.5" />
          Retry
        </Button>
      ) : null}
    </div>
  );
}

export function EmptyState({
  title,
  description,
  action,
  className,
}: {
  title: string;
  description?: string;
  action?: React.ReactNode;
  className?: string;
}) {
  return (
    <div className={cn('relative flex min-w-0 flex-col items-center gap-2 overflow-hidden rounded-2xl border border-dashed border-white/[0.08] bg-white/[0.015] p-6 text-center sm:p-10', className)}>
      <DotPattern className="fill-white/[0.05] [mask-image:radial-gradient(320px_circle_at_center,white,transparent)]" />
      <div className="relative z-10 flex flex-col items-center gap-2">
        <Inbox className="size-6 text-neutral-500" />
        <p className="text-sm font-medium text-white">{title}</p>
        {description ? <p className="max-w-md text-xs text-neutral-400">{description}</p> : null}
        {action ? <div className="mt-2">{action}</div> : null}
      </div>
    </div>
  );
}

/** A table-shaped loading placeholder. */
export function TableSkeleton({ rows = 6, columns = 5 }: { rows?: number; columns?: number }) {
  return (
    <div className="space-y-2 p-5">
      {Array.from({ length: rows }).map((_, rowIndex) => (
        <div key={rowIndex} className="flex gap-3">
          {Array.from({ length: columns }).map((__, columnIndex) => (
            <Skeleton
              key={columnIndex}
              className="h-5 flex-1"
              // The first column is an identifier and reads as longer; varying
              // the widths keeps the placeholder from looking like a table of
              // identical bars.
              style={{ flexGrow: columnIndex === 0 ? 2 : 1 }}
            />
          ))}
        </div>
      ))}
    </div>
  );
}

export function CardsSkeleton({ count = 4 }: { count?: number }) {
  return (
    <div className="grid gap-4 sm:grid-cols-2 xl:grid-cols-4">
      {Array.from({ length: count }).map((_, index) => (
        <Skeleton key={index} className="h-28" />
      ))}
    </div>
  );
}

export function ChartSkeleton({ height = 240 }: { height?: number }) {
  return <Skeleton style={{ height }} />;
}
