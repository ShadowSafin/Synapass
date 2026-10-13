'use client';

import { ChevronDown, ListPlus, RefreshCw, RotateCw, Trash2 } from 'lucide-react';
import * as React from 'react';

import { PageHeader } from '@/components/page-header';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input, Select, fieldClasses } from '@/components/ui/input';
import { EmptyState, ErrorState, TableSkeleton } from '@/components/ui/state';
import { useCreatePolicy, useDeletePolicy, usePolicies, useReloadPolicies, useUpsertPolicy } from '@/hooks/use-admin';
import { ApiError } from '@/lib/api';
import { formatCurrency, formatDurationMs, formatNumber } from '@/lib/format';
import { cn } from '@/lib/utils';
import type { RoutingPolicy } from '@/lib/types';

const STRATEGY_OPTIONS = ['priority', 'weighted', 'lowest_cost', 'lowest_latency', 'highest_quality'];

function parseJsonField(label: string, value: string): { ok: boolean; parsed?: unknown; error?: string } {
  const trimmed = value.trim();
  if (!trimmed) return { ok: true, parsed: undefined };
  try {
    return { ok: true, parsed: JSON.parse(trimmed) as unknown };
  } catch (error) {
    return { ok: false, error: `${label}: ${error instanceof Error ? error.message : 'invalid JSON'}` };
  }
}

function stringify(value: unknown): string {
  if (value === undefined || value === null) return '';
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return '';
  }
}

interface PolicyFormState {
  name: string;
  description: string;
  priority: string;
  enabled: boolean;
  strategy: string;
  match: string;
  targets: string;
  fallback: string;
  retry: string;
  timeout: string;
  limits: string;
}

function formFromPolicy(policy?: RoutingPolicy): PolicyFormState {
  return {
    name: policy?.name ?? '',
    description: policy?.description ?? '',
    priority: policy ? String(policy.priority) : '100',
    enabled: policy?.enabled ?? true,
    strategy: policy?.strategy ?? 'priority',
    match: policy ? stringify(policy.match) : '{\n  "models": []\n}',
    targets: policy ? stringify(policy.targets ?? []) : '[]',
    fallback: policy ? stringify(policy.fallback) : '{\n  "enabled": true,\n  "max_attempts": 2,\n  "budget_aware": false\n}',
    retry: policy ? stringify(policy.retry) : '{\n  "max_attempts": 1\n}',
    timeout: policy ? stringify(policy.timeout) : '{\n  "total": 60000000000,\n  "per_attempt": 30000000000\n}',
    limits: policy ? stringify(policy.limits) : '{}',
  };
}

function PolicyForm({ initial, isEdit, onClose }: { initial: PolicyFormState; isEdit: boolean; onClose: () => void }) {
  const createPolicy = useCreatePolicy();
  const upsertPolicy = useUpsertPolicy();
  const [form, setForm] = React.useState<PolicyFormState>(initial);
  const [formError, setFormError] = React.useState<string | null>(null);
  const [success, setSuccess] = React.useState<string | null>(null);

  const setText =
    (key: keyof PolicyFormState) =>
    (event: React.ChangeEvent<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>) => {
      setForm((current) => ({ ...current, [key]: event.target.value }));
    };

  const onSubmit = (event: React.FormEvent) => {
    event.preventDefault();
    setFormError(null);
    setSuccess(null);

    if (!form.name.trim()) {
      setFormError('name is required — it is the stable identity used for upserts');
      return;
    }
    const priority = Number(form.priority);
    if (!Number.isFinite(priority)) {
      setFormError('priority must be a number');
      return;
    }
    const match = parseJsonField('match', form.match);
    if (!match.ok) {
      setFormError(match.error ?? 'invalid match JSON');
      return;
    }
    const targets = parseJsonField('targets', form.targets);
    if (!targets.ok) {
      setFormError(targets.error ?? 'invalid targets JSON');
      return;
    }
    const fallback = parseJsonField('fallback', form.fallback);
    if (!fallback.ok) {
      setFormError(fallback.error ?? 'invalid fallback JSON');
      return;
    }
    const retry = parseJsonField('retry', form.retry);
    if (!retry.ok) {
      setFormError(retry.error ?? 'invalid retry JSON');
      return;
    }
    const timeout = parseJsonField('timeout', form.timeout);
    if (!timeout.ok) {
      setFormError(timeout.error ?? 'invalid timeout JSON');
      return;
    }
    const limits = parseJsonField('limits', form.limits);
    if (!limits.ok) {
      setFormError(limits.error ?? 'invalid limits JSON');
      return;
    }
    if (match.parsed !== undefined && (typeof match.parsed !== 'object' || match.parsed === null || Array.isArray(match.parsed))) {
      setFormError('match must be a JSON object');
      return;
    }
    if (targets.parsed !== undefined && !Array.isArray(targets.parsed)) {
      setFormError('targets must be a JSON array');
      return;
    }

    const body = {
      name: form.name.trim(),
      description: form.description.trim() || undefined,
      priority,
      enabled: form.enabled,
      strategy: form.strategy,
      match: (match.parsed ?? {}) as RoutingPolicy['match'],
      targets: (targets.parsed ?? []) as RoutingPolicy['targets'],
      fallback: fallback.parsed as RoutingPolicy['fallback'],
      retry: retry.parsed as RoutingPolicy['retry'],
      timeout: timeout.parsed as RoutingPolicy['timeout'],
      limits: limits.parsed as RoutingPolicy['limits'],
    };

    const mutation = isEdit ? upsertPolicy : createPolicy;
    mutation.mutate(body, {
      onSuccess: () => {
        setSuccess(isEdit ? 'saved' : 'created');
        if (!isEdit) setForm(formFromPolicy(undefined));
        window.setTimeout(onClose, isEdit ? 1000 : 1500);
      },
      onError: (mutationError) =>
        setFormError(mutationError instanceof ApiError ? mutationError.message : 'the policy could not be saved'),
    });
  };

  const pending = createPolicy.isPending || upsertPolicy.isPending;

  return (
    <form onSubmit={onSubmit} className="space-y-4">
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Name (upsert key)
          <Input value={form.name} onChange={setText('name')} placeholder="default-chat" disabled={isEdit} />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Priority
          <Input value={form.priority} onChange={setText('priority')} inputMode="numeric" placeholder="100" />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Strategy
          <Select value={form.strategy} onChange={setText('strategy')}>
            {STRATEGY_OPTIONS.map((strategy) => (
              <option key={strategy} value={strategy}>
                {strategy}
              </option>
            ))}
          </Select>
        </label>
        <label className="flex flex-row items-center gap-2 text-xs text-muted-foreground">
          <input
            type="checkbox"
            checked={form.enabled}
            onChange={(event) => setForm((current) => ({ ...current, enabled: event.target.checked }))}
            className="size-4"
          />
          Enabled
        </label>
      </div>
      <label className="flex flex-col gap-1 text-xs text-muted-foreground">
        Description
        <Input value={form.description} onChange={setText('description')} placeholder="what this policy routes and why" />
      </label>
      <div className="grid gap-3 lg:grid-cols-2">
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Match (JSON object)
          <textarea value={form.match} onChange={setText('match')} rows={5} spellCheck={false} className={cn(fieldClasses, 'h-auto font-mono text-xs')} />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Targets (JSON array)
          <textarea value={form.targets} onChange={setText('targets')} rows={5} spellCheck={false} className={cn(fieldClasses, 'h-auto font-mono text-xs')} />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Fallback (JSON object)
          <textarea value={form.fallback} onChange={setText('fallback')} rows={4} spellCheck={false} className={cn(fieldClasses, 'h-auto font-mono text-xs')} />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Retry (JSON object)
          <textarea value={form.retry} onChange={setText('retry')} rows={4} spellCheck={false} className={cn(fieldClasses, 'h-auto font-mono text-xs')} />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Timeout (JSON object)
          <textarea value={form.timeout} onChange={setText('timeout')} rows={4} spellCheck={false} className={cn(fieldClasses, 'h-auto font-mono text-xs')} />
        </label>
        <label className="flex flex-col gap-1 text-xs text-muted-foreground">
          Limits (JSON object)
          <textarea value={form.limits} onChange={setText('limits')} rows={4} spellCheck={false} className={cn(fieldClasses, 'h-auto font-mono text-xs')} />
        </label>
      </div>
      {formError ? <p className="text-xs text-danger">{formError}</p> : null}
      {success ? <p className="text-xs text-success">{success}</p> : null}
      <div className="flex gap-2">
        <Button type="submit" size="sm" disabled={pending}>
          {pending ? 'Saving…' : isEdit ? 'Save changes' : 'Create policy'}
        </Button>
        <Button type="button" variant="ghost" size="sm" onClick={onClose}>
          Cancel
        </Button>
      </div>
    </form>
  );
}

function PolicyCard({ policy }: { policy: RoutingPolicy }) {
  const [open, setOpen] = React.useState(false);
  const [editing, setEditing] = React.useState(false);
  const [feedback, setFeedback] = React.useState<string | null>(null);
  const deletePolicy = useDeletePolicy();

  const targets = policy.targets ?? [];
  const matchParts: string[] = [];
  if ((policy.match.models ?? []).length > 0) matchParts.push(`models: ${(policy.match.models ?? []).join(', ')}`);
  if ((policy.match.request_types ?? []).length > 0) {
    matchParts.push(`types: ${(policy.match.request_types ?? []).join(', ')}`);
  }
  if ((policy.match.tenant_ids ?? []).length > 0) {
    matchParts.push(`${(policy.match.tenant_ids ?? []).length} tenant(s)`);
  }
  // Phase 2 match dimensions.
  const extra = policy.match as Record<string, unknown>;
  if (Array.isArray(extra.endpoint_ids) && extra.endpoint_ids.length > 0) matchParts.push(`endpoints: ${(extra.endpoint_ids as string[]).join(', ')}`);
  if (Array.isArray(extra.task_types) && extra.task_types.length > 0) matchParts.push(`tasks: ${(extra.task_types as string[]).join(', ')}`);
  if (Array.isArray(extra.regions) && extra.regions.length > 0) matchParts.push(`regions: ${(extra.regions as string[]).join(', ')}`);
  if (policy.match.min_prompt_tokens) matchParts.push(`≥ ${formatNumber(policy.match.min_prompt_tokens)} prompt tokens`);
  if (policy.match.max_prompt_tokens) matchParts.push(`≤ ${formatNumber(policy.match.max_prompt_tokens)} prompt tokens`);
  if (policy.match.streaming !== undefined) matchParts.push(policy.match.streaming ? 'streaming only' : 'non-streaming only');
  if (matchParts.length === 0) matchParts.push('every request (catch-all)');

  const onDelete = () => {
    if (!window.confirm(`Delete policy "${policy.name}"? Requests that matched it fall through to the next rule.`)) return;
    setFeedback(null);
    deletePolicy.mutate(policy.id, {
      onSuccess: () => setFeedback('deleted'),
      onError: (e) => setFeedback(e instanceof ApiError ? e.message : 'delete failed'),
    });
  };

  return (
    <Card>
      <CardHeader>
        <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-start sm:justify-between">
          <div className="min-w-0 space-y-1">
            <CardTitle className="flex flex-wrap items-center gap-2 text-base">
              {policy.name}
              <Badge tone={policy.enabled ? 'success' : 'neutral'} dot>
                {policy.enabled ? 'enabled' : 'disabled'}
              </Badge>
              <Badge tone="outline">{policy.strategy}</Badge>
              <Badge tone="neutral">priority {policy.priority}</Badge>
              <Badge tone="outline">v{policy.version}</Badge>
            </CardTitle>
            {policy.description ? <CardDescription>{policy.description}</CardDescription> : null}
            <p className="text-xs text-muted-foreground">Matches {matchParts.join(' · ')}</p>
            {feedback ? <p className="text-xs text-info">{feedback}</p> : null}
          </div>
          <div className="flex flex-wrap gap-1">
            <Button variant="ghost" size="sm" onClick={() => setOpen((value) => !value)} aria-expanded={open}>
              <ChevronDown className={cn('transition-transform', open ? 'rotate-180' : undefined)} />
              {open ? 'Hide' : 'Details'}
            </Button>
            <Button variant="outline" size="sm" onClick={() => setEditing((value) => !value)}>
              {editing ? 'Close' : 'Edit'}
            </Button>
            <Button variant="ghost" size="sm" onClick={onDelete} disabled={deletePolicy.isPending}>
              <Trash2 />
              Delete
            </Button>
          </div>
        </div>
      </CardHeader>

      <CardContent className="space-y-4">
        {editing ? (
          <div className="rounded-md border border-border p-3">
            <PolicyForm initial={formFromPolicy(policy)} isEdit onClose={() => setEditing(false)} />
          </div>
        ) : null}
        <div>
          <p className="mb-2 text-xs font-medium uppercase tracking-wide text-muted-foreground">
            Fallback chain (in order)
          </p>
          <ol className="space-y-1.5">
            {targets.map((target, index) => (
              <li key={`${target.provider_name ?? target.provider_id ?? 'target'}-${target.model}-${index}`} className="flex items-center gap-2 text-xs">
                <span className="flex size-5 shrink-0 items-center justify-center rounded-full bg-muted text-[10px] font-medium">
                  {index + 1}
                </span>
                <span className="font-medium">{target.provider_name || target.provider_id || 'any provider'}</span>
                <span className="font-mono text-muted-foreground">{target.model}</span>
                {target.weight ? <Badge tone="outline">weight {target.weight}</Badge> : null}
                {index > 0 ? <Badge tone="warning">fallback</Badge> : null}
              </li>
            ))}
          </ol>
        </div>

        {open ? (
          <div className="grid gap-3 border-t border-border pt-4 sm:grid-cols-2 lg:grid-cols-4">
            <Detail label="Fallback">
              {policy.fallback?.enabled ? (
                <>
                  up to {policy.fallback.max_attempts} provider(s)
                  {policy.fallback.budget_aware ? ' · budget-aware' : ''}
                </>
              ) : (
                'disabled — the primary target must answer'
              )}
            </Detail>
            <Detail label="Retry">
              {policy.retry?.max_attempts ?? 1} attempt(s) per provider
            </Detail>
            <Detail label="Timeout">
              total {formatDurationMs((policy.timeout?.total ?? 0) / 1_000_000)}
              {policy.timeout?.per_attempt ? ` · per attempt ${formatDurationMs(policy.timeout.per_attempt / 1_000_000)}` : ''}
            </Detail>
            <Detail label="Rate limit">
              {policy.limits?.requests_per_minute
                ? `${formatNumber(policy.limits.requests_per_minute)} req/min`
                : 'inherits the platform default'}
            </Detail>
            <Detail label="Max cost / request">
              {policy.limits?.max_cost_per_request_usd
                ? formatCurrency(policy.limits.max_cost_per_request_usd)
                : 'unlimited'}
            </Detail>
            <Detail label="Output cap">
              {policy.limits?.max_output_tokens ? `${formatNumber(policy.limits.max_output_tokens)} tokens` : 'model default'}
            </Detail>
            <Detail label="Daily budget">
              {policy.limits?.daily_budget_usd ? formatCurrency(policy.limits.daily_budget_usd) : 'none'}
            </Detail>
            <Detail label="Latency target">
              {policy.limits?.latency_target_ms ? formatDurationMs(policy.limits.latency_target_ms) : 'none'}
            </Detail>
          </div>
        ) : null}
      </CardContent>
    </Card>
  );
}

function Detail({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="space-y-0.5">
      <p className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">{label}</p>
      <p className="text-xs">{children}</p>
    </div>
  );
}

export function PoliciesView() {
  const { data: policies, isPending, isError, error, refetch, isFetching } = usePolicies();
  const reload = useReloadPolicies();
  const [feedback, setFeedback] = React.useState<string | null>(null);
  const [showCreate, setShowCreate] = React.useState(false);

  const onReload = () => {
    setFeedback(null);
    reload.mutate(undefined, {
      onSuccess: (result) => setFeedback(`reloaded ${result.reloaded} polic${result.reloaded === 1 ? 'y' : 'ies'}`),
      onError: (mutationError) =>
        setFeedback(mutationError instanceof ApiError ? mutationError.message : 'the reload failed'),
    });
  };

  const sorted = React.useMemo(
    () => [...(policies ?? [])].sort((a, b) => a.priority - b.priority || a.name.localeCompare(b.name)),
    [policies],
  );

  return (
    <>
      <PageHeader
        title="Policies"
        description="How a request chooses a provider and what happens when that choice fails. The resolver sorts matching policies by specificity, then by priority, so the most specific rule always wins regardless of insertion order."
        actions={
          <>
            <Button size="sm" onClick={() => setShowCreate((value) => !value)}>
              <ListPlus />
              {showCreate ? 'Hide form' : 'Create policy'}
            </Button>
            <Button variant="outline" size="sm" onClick={onReload} disabled={reload.isPending}>
              <RotateCw className={reload.isPending ? 'animate-spin' : undefined} />
              Reload cache
            </Button>
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching}>
              <RefreshCw className={isFetching ? 'animate-spin' : undefined} />
              Refresh
            </Button>
          </>
        }
        note={feedback ? <span className="text-info">{feedback}</span> : undefined}
      />

      {isError ? <ErrorState error={error} onRetry={() => void refetch()} /> : null}

      {showCreate ? (
        <Card className="mb-4">
          <CardHeader>
            <CardTitle>Create policy</CardTitle>
            <CardDescription>
              Scalar fields are edited directly; match, targets, fallback, retry, timeout and limits are JSON. Names
              must be unique per scope — duplicates return a 409.
            </CardDescription>
          </CardHeader>
          <CardContent>
            <PolicyForm initial={formFromPolicy(undefined)} isEdit={false} onClose={() => setShowCreate(false)} />
          </CardContent>
        </Card>
      ) : null}

      {isPending ? (
        <TableSkeleton rows={4} columns={3} />
      ) : sorted.length === 0 ? (
        <EmptyState
          title="No policies defined"
          description="Without a policy the gateway falls back to its routing defaults. Create one above or define policies in the config's `policies` section for explicit, versioned routing."
        />
      ) : (
        <div className="space-y-4">
          {sorted.map((policy) => (
            <PolicyCard key={policy.id || policy.name} policy={policy} />
          ))}
        </div>
      )}
    </>
  );
}
