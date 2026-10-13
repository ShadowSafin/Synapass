'use client';

import { usePathname, useSearchParams } from 'next/navigation';

import { useTenants } from '@/hooks/use-admin';

/** The tenant currently selected in the URL, or an empty string for "all". */
export function useTenantParam(): string {
  const params = useSearchParams();
  return params.get('tenant') ?? '';
}

/**
 * A tenant filter.
 *
 * Selection lives in the URL so a filtered view is linkable. "All tenants" is a
 * first-class option rather than an empty state, because the platform overview
 * is the default view an operator wants.
 */
export function TenantPicker({ value, allowAll = true }: { value: string; allowAll?: boolean }) {
  const pathname = usePathname();
  const params = useSearchParams();
  const { data: tenants, isLoading, error } = useTenants();

  const hrefFor = (next: string): string => {
    const search = new URLSearchParams(params.toString());
    if (next) {
      search.set('tenant', next);
    } else {
      search.delete('tenant');
    }
    return `${pathname}?${search.toString()}`;
  };

  // A tenant list that cannot be read should not block the page: the picker
  // degrades to "all tenants", which is a valid selection.
  if (error) {
    return <span className="text-xs text-muted-foreground">Tenant list unavailable</span>;
  }

  if (isLoading) {
    return <span className="text-xs text-muted-foreground">Loading tenants…</span>;
  }

  const options = allowAll ? [{ id: '', name: 'All tenants' }, ...(tenants ?? [])] : (tenants ?? []);

  return (
    <label className="inline-flex min-w-0 max-w-full items-center gap-2 text-xs text-neutral-400">
      <span className="sr-only">Tenant</span>
      <select
        value={value}
        onChange={(event) => {
          window.location.assign(hrefFor(event.target.value));
        }}
        className="h-8.5 max-w-[calc(100vw-2rem)] truncate rounded-xl border border-white/[0.08] bg-[#121217] px-3 text-xs text-neutral-200 focus:outline-none focus:ring-1 focus:ring-primary/50 shadow-sm sm:max-w-[220px]"
      >
        {options.map((tenant) => (
          <option key={tenant.id || 'all'} value={tenant.id}>
            {tenant.name}
          </option>
        ))}
      </select>
    </label>
  );
}
