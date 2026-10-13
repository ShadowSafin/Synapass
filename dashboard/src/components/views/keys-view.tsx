'use client';

import { AlertTriangle, BookKey, Check, Copy, Pencil, Plus, RefreshCw, RotateCw, Trash2, X } from 'lucide-react';
import Link from 'next/link';
import * as React from 'react';

import { PageHeader } from '@/components/page-header';
import { TenantPicker, useTenantParam } from '@/components/tenant-picker';
import { Badge } from '@/components/ui/badge';
import { Button } from '@/components/ui/button';
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card';
import { Input } from '@/components/ui/input';
import { EmptyState, ErrorState, TableSkeleton } from '@/components/ui/state';
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table';
import { useCreateKey, useKeys, useRevokeKey, useRotateKey, useTenants, useUpdateKey } from '@/hooks/use-admin';
import { ApiError } from '@/lib/api';
import { formatDateTime, formatRelative } from '@/lib/format';
import type { APIKey } from '@/lib/types';

/** The scopes an operator can grant, in the order they should be offered. */
const SCOPE_OPTIONS = [
  { value: 'inference', label: 'inference', hint: 'call /v1/chat/completions' },
  { value: 'usage:read', label: 'usage:read', hint: 'read own usage and logs' },
  { value: 'models:read', label: 'models:read', hint: 'list the model registry' },
  { value: 'policies:admin', label: 'policies:admin', hint: 'manage routing policies' },
  { value: 'providers:admin', label: 'providers:admin', hint: 'view and probe providers' },
  { value: 'keys:admin', label: 'keys:admin', hint: 'mint and revoke keys' },
  { value: 'tenants:admin', label: 'tenants:admin', hint: 'cross-tenant administration' },
] as const;

function KeyRow({
  apiKey,
  tenantId,
  onRotated,
}: {
  apiKey: APIKey;
  tenantId: string;
  onRotated: (plaintext: string, name: string, warning?: string) => void;
}) {
  const revokeKey = useRevokeKey();
  const rotateKey = useRotateKey();
  const updateKey = useUpdateKey();
  const [editing, setEditing] = React.useState(false);
  const [name, setName] = React.useState(apiKey.name);
  const [scopes, setScopes] = React.useState<string[]>(apiKey.scopes ?? []);
  const [expiresAt, setExpiresAt] = React.useState(apiKey.expires_at ?? '');
  const [routingPolicyId, setRoutingPolicyId] = React.useState(apiKey.routing_policy_id ?? '');
  const [feedback, setFeedback] = React.useState<string | null>(null);

  const toggleScope = (scope: string) => {
    setScopes((current) =>
      current.includes(scope) ? current.filter((item) => item !== scope) : [...current, scope],
    );
  };

  const onSave = (event: React.FormEvent) => {
    event.preventDefault();
    setFeedback(null);
    if (!name.trim()) {
      setFeedback('name is required');
      return;
    }
    updateKey.mutate(
      {
        id: apiKey.id,
        tenantId,
        body: {
          name: name.trim(),
          scopes,
          // Blank clears the expiry; otherwise the raw RFC3339 string is sent.
          expires_at: expiresAt.trim() ? expiresAt.trim() : null,
          routing_policy_id: routingPolicyId.trim() ? routingPolicyId.trim() : null,
        },
      },
      {
        onSuccess: () => {
          setFeedback('saved');
          setEditing(false);
        },
        onError: (e) => setFeedback(e instanceof ApiError ? e.message : 'the key could not be updated'),
      },
    );
  };

  const onRotate = () => {
    if (!window.confirm(`Rotate "${apiKey.name}" (${apiKey.prefix})? The current credential stops working immediately.`)) {
      return;
    }
    setFeedback(null);
    rotateKey.mutate(
      { id: apiKey.id, tenantId },
      {
        onSuccess: (result) => onRotated(result.plaintext, apiKey.name, result.warning),
        onError: (e) => setFeedback(e instanceof ApiError ? e.message : 'the key could not be rotated'),
      },
    );
  };

  return (
    <TableRow>
      <TableCell>
        <p className="text-sm font-medium">{apiKey.name}</p>
        {apiKey.created_by ? <p className="text-[11px] text-muted-foreground">by {apiKey.created_by}</p> : null}
        {feedback ? <p className="text-[11px] text-info">{feedback}</p> : null}
        {editing ? (
          <form onSubmit={onSave} className="mt-2 max-w-lg space-y-3 rounded-md border border-border p-3">
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              Name
              <Input value={name} onChange={(event) => setName(event.target.value)} />
            </label>
            <div className="space-y-1">
              <p className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Scopes</p>
              <div className="grid gap-1 sm:grid-cols-2">
                {SCOPE_OPTIONS.map((option) => (
                  <label key={option.value} className="flex cursor-pointer items-center gap-2 text-xs">
                    <input
                      type="checkbox"
                      checked={scopes.includes(option.value)}
                      onChange={() => toggleScope(option.value)}
                      className="size-3.5"
                    />
                    <span className="font-mono">{option.label}</span>
                  </label>
                ))}
              </div>
            </div>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              Expires at (RFC3339, blank = never)
              <Input
                value={expiresAt}
                onChange={(event) => setExpiresAt(event.target.value)}
                placeholder="2027-01-01T00:00:00Z"
              />
            </label>
            <label className="flex flex-col gap-1 text-xs text-muted-foreground">
              Routing policy id (blank = default)
              <Input
                value={routingPolicyId}
                onChange={(event) => setRoutingPolicyId(event.target.value)}
                placeholder="policy id"
              />
            </label>
            <div className="flex gap-2">
              <Button type="submit" size="sm" disabled={updateKey.isPending}>
                {updateKey.isPending ? 'Saving…' : 'Save'}
              </Button>
              <Button type="button" variant="ghost" size="sm" onClick={() => setEditing(false)}>
                Cancel
              </Button>
            </div>
          </form>
        ) : null}
      </TableCell>
      <TableCell className="font-mono text-xs">{apiKey.prefix}</TableCell>
      <TableCell>
        <div className="flex max-w-xs flex-wrap gap-1">
          {(apiKey.scopes ?? []).map((scope) => (
            <Badge key={scope} tone={scope === '*' ? 'danger' : 'outline'}>
              {scope}
            </Badge>
          ))}
        </div>
      </TableCell>
      <TableCell>
        <Badge tone={apiKey.status === 'active' ? 'success' : 'neutral'} dot>
          {apiKey.status}
        </Badge>
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">
        {apiKey.last_used_at ? formatRelative(apiKey.last_used_at) : 'never'}
      </TableCell>
      <TableCell className="text-xs text-muted-foreground">
        {apiKey.expires_at ? formatDateTime(apiKey.expires_at) : 'never'}
      </TableCell>
      <TableCell className="text-right">
        <div className="flex flex-wrap justify-end gap-1">
          <Button variant="outline" size="sm" onClick={() => setEditing((value) => !value)} disabled={apiKey.status !== 'active'}>
            <Pencil />
            {editing ? 'Close' : 'Edit'}
          </Button>
          <Button variant="outline" size="sm" onClick={onRotate} disabled={apiKey.status !== 'active' || rotateKey.isPending}>
            <RotateCw className={rotateKey.isPending ? 'animate-spin' : undefined} />
            {rotateKey.isPending ? 'Rotating…' : 'Rotate'}
          </Button>
          <Button
            variant="ghost"
            size="sm"
            disabled={apiKey.status !== 'active' || revokeKey.isPending}
            onClick={() => {
              if (
                window.confirm(
                  `Revoke "${apiKey.name}" (${apiKey.prefix})? Any client using it will start receiving 401s immediately.`,
                )
              ) {
                revokeKey.mutate({ id: apiKey.id, tenantId });
              }
            }}
          >
            <Trash2 />
            Revoke
          </Button>
        </div>
      </TableCell>
    </TableRow>
  );
}

export function KeysView() {
  const tenantParam = useTenantParam();
  const { data: tenants, isPending: tenantsPending } = useTenants();
  // The URL selects the tenant. When it is absent we fall back to the first
  // tenant rather than to nothing: otherwise the picker displays a tenant while
  // the page treats none as selected, and with a single tenant the operator
  // cannot change a one-option select to trigger the load. That left the page
  // looking read-only, with no list and no create form.
  const tenantId = tenantParam || tenants?.[0]?.id || '';
  const tenantName = tenants?.find((tenant) => tenant.id === tenantId)?.name;
  const { data: keys, isPending, isError, error, refetch, isFetching } = useKeys(tenantId);
  const createKey = useCreateKey();
  const revokeKey = useRevokeKey();

  const [name, setName] = React.useState('');
  const [scopes, setScopes] = React.useState<string[]>(['inference']);
  const [expiresInHours, setExpiresInHours] = React.useState('');
  const [minted, setMinted] = React.useState<{
    plaintext: string;
    name: string;
    warning?: string;
    createdAt?: string;
    tenantName?: string;
  } | null>(null);
  const [copied, setCopied] = React.useState(false);
  const [formError, setFormError] = React.useState<string | null>(null);
  const [formOpen, setFormOpen] = React.useState(false);
  const formRef = React.useRef<HTMLDivElement>(null);

  const openForm = React.useCallback(() => {
    setFormOpen(true);
    // The form may be below the fold; bring it into view for the operator.
    window.requestAnimationFrame(() =>
      formRef.current?.scrollIntoView({ behavior: 'smooth', block: 'start' }),
    );
  }, []);

  const toggleScope = (scope: string) => {
    setScopes((current) =>
      current.includes(scope) ? current.filter((item) => item !== scope) : [...current, scope],
    );
  };

  const onCreate = (event: React.FormEvent) => {
    event.preventDefault();
    setFormError(null);

    if (!tenantId) {
      setFormError('select a tenant first — a key must belong to one');
      return;
    }
    if (!name.trim()) {
      setFormError('give the key a name so it can be recognised later');
      return;
    }
    if (scopes.length === 0) {
      setFormError('grant at least one scope, or the key can do nothing');
      return;
    }

    const hours = expiresInHours.trim() ? Number(expiresInHours) : undefined;
    if (hours !== undefined && (!Number.isFinite(hours) || hours <= 0)) {
      setFormError('expiry must be a positive number of hours, or left blank to never expire');
      return;
    }

    createKey.mutate(
      { tenantId, name: name.trim(), scopes, expiresInHours: hours },
      {
        onSuccess: (result) => {
          setMinted({
            plaintext: result.plaintext,
            name: name.trim(),
            warning: result.warning,
            createdAt: result.key.created_at,
            tenantName,
          });
          setName('');
          setExpiresInHours('');
          setFormOpen(false);
        },
        onError: (mutationError) => {
          setFormError(mutationError instanceof ApiError ? mutationError.message : 'the key could not be created');
        },
      },
    );
  };

  const onCopy = async () => {
    if (!minted) return;
    try {
      await navigator.clipboard.writeText(minted.plaintext);
      setCopied(true);
      window.setTimeout(() => setCopied(false), 2000);
    } catch {
      // Clipboard access can be denied; the value stays selectable on screen.
      setCopied(false);
    }
  };

  return (
    <>
      <PageHeader
        title="API keys"
        description="Credentials presented as bearer tokens on inference calls. The gateway stores only a SHA-256 digest, so a leaked database yields no usable key — which also means an existing key can never be displayed again."
        actions={
          <>
            <TenantPicker value={tenantId} allowAll={false} />
            <Button variant="outline" size="sm" onClick={() => void refetch()} disabled={isFetching || !tenantId}>
              <RefreshCw className={isFetching ? 'animate-spin' : undefined} />
              Refresh
            </Button>
            <Button
              size="sm"
              onClick={openForm}
              disabled={!tenantId}
              title={!tenantId ? 'Create or select a tenant first — keys cannot be global' : undefined}
            >
              <Plus />
              Create API key
            </Button>
          </>
        }
      />

      {minted ? (
        <Card className="mb-4 border-warning/50">
          <CardHeader>
            <CardTitle className="flex items-center gap-2 text-base">
              <AlertTriangle className="size-4 text-warning" />
              Copy this key now — it will never be shown again
            </CardTitle>
            <CardDescription>
              The gateway returned it once and keeps only the hash. If it is lost, revoke it and mint another.
              {minted.warning ? ` ${minted.warning}` : null}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-3">
            <div className="flex min-w-0 flex-col gap-2 sm:flex-row sm:flex-wrap sm:items-center">
              <code className="min-w-0 flex-1 break-all rounded-md border border-border bg-muted px-3 py-2 font-mono text-xs">
                {minted.plaintext}
              </code>
              <Button variant="secondary" size="sm" onClick={() => void onCopy()} className="shrink-0">
                {copied ? <Check /> : <Copy />}
                {copied ? 'Copied' : 'Copy'}
              </Button>
            </div>
            <p className="text-xs text-muted-foreground">
              For <span className="font-medium">{minted.name}</span>
              {minted.tenantName ? (
                <>
                  {' '}
                  in <span className="font-medium">{minted.tenantName}</span>
                </>
              ) : null}
              {minted.createdAt ? <> · created {formatDateTime(minted.createdAt)}</> : null}. Use it as{' '}
              <code className="font-mono">Authorization: Bearer &lt;key&gt;</code>.
            </p>
            <Button variant="ghost" size="sm" onClick={() => setMinted(null)}>
              Dismiss
            </Button>
          </CardContent>
        </Card>
      ) : null}

      {!tenantsPending && (tenants ?? []).length === 0 ? (
        <Card className="mb-4">
          <CardContent className="space-y-3 pt-5 sm:pt-6 text-sm text-muted-foreground">
            <p>
              No tenants exist yet. Every API key belongs to a tenant and there is no global scope, so create a
              tenant first and its keys can be minted here.
            </p>
            <Button asChild variant="outline" size="sm">
              <Link href="/tenants">Go to tenants</Link>
            </Button>
          </CardContent>
        </Card>
      ) : null}

      {formOpen && tenantId ? (
        <Card className="mb-4" ref={formRef}>
          <CardHeader>
            <div className="flex items-start justify-between gap-4">
              <div className="space-y-1.5">
                <CardTitle className="flex items-center gap-2">
                  <Plus className="size-4" />
                  Create an API key
                </CardTitle>
                <CardDescription>
                  A key with no scope would be unusable, so at least one is required. Inference is the
                  least-privilege default. It will belong to{' '}
                  <span className="font-medium text-foreground">{tenantName ?? 'the selected tenant'}</span>.
                </CardDescription>
              </div>
              <Button
                type="button"
                variant="ghost"
                size="icon"
                aria-label="Close the create form"
                onClick={() => setFormOpen(false)}
              >
                <X />
              </Button>
            </div>
          </CardHeader>
          <CardContent>
            <form onSubmit={onCreate} className="space-y-4">
              <div className="grid gap-4 sm:grid-cols-2">
                <label className="flex flex-col gap-1 text-xs text-muted-foreground">
                  Name
                  <Input value={name} onChange={(event) => setName(event.target.value)} placeholder="e.g. payments-service" />
                </label>
                <label className="flex flex-col gap-1 text-xs text-muted-foreground">
                  Expires in (hours, blank = never)
                  <Input
                    value={expiresInHours}
                    onChange={(event) => setExpiresInHours(event.target.value)}
                    placeholder="720"
                    inputMode="numeric"
                  />
                </label>
              </div>

              <fieldset className="space-y-2">
                <legend className="text-xs font-medium uppercase tracking-wide text-muted-foreground">Scopes</legend>
                <div className="grid gap-2 sm:grid-cols-2 lg:grid-cols-3">
                  {SCOPE_OPTIONS.map((option) => (
                    <label
                      key={option.value}
                      className="flex cursor-pointer items-start gap-2 rounded-md border border-border p-2 text-xs hover:bg-accent"
                    >
                      <input
                        type="checkbox"
                        checked={scopes.includes(option.value)}
                        onChange={() => toggleScope(option.value)}
                        className="mt-0.5 size-3.5"
                      />
                      <span>
                        <span className="font-mono font-medium">{option.label}</span>
                        <span className="block text-muted-foreground">{option.hint}</span>
                      </span>
                    </label>
                  ))}
                </div>
              </fieldset>

              {formError ? <p className="text-xs text-danger">{formError}</p> : null}

              <Button type="submit" disabled={createKey.isPending}>
                <BookKey />
                {createKey.isPending ? 'Creating…' : 'Create API key'}
              </Button>
            </form>
          </CardContent>
        </Card>
      ) : null}

      {isError ? <ErrorState error={error} onRetry={() => void refetch()} /> : null}

      {tenantId ? (
        <Card>
          <CardContent className="p-0">
            {isPending ? (
              <TableSkeleton rows={4} columns={6} />
            ) : (keys ?? []).length === 0 ? (
              <EmptyState
                title="No keys for this tenant"
                description="A tenant with no keys cannot call the inference API. Create one to get started."
                className="m-5"
                action={
                  <Button size="sm" onClick={openForm}>
                    <Plus />
                    Create API key
                  </Button>
                }
              />
            ) : (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>Name</TableHead>
                    <TableHead>Prefix</TableHead>
                    <TableHead>Scopes</TableHead>
                    <TableHead>Status</TableHead>
                    <TableHead>Last used</TableHead>
                    <TableHead>Expires</TableHead>
                    <TableHead />
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {(keys ?? []).map((key) => (
                    <KeyRow
                      key={key.id}
                      apiKey={key}
                      tenantId={tenantId}
                      onRotated={(plaintext, keyName, warning) => setMinted({ plaintext, name: keyName, warning })}
                    />
                  ))}
                </TableBody>
              </Table>
            )}
          </CardContent>
        </Card>
      ) : null}

      {revokeKey.isError ? (
        <div className="mt-4">
          <ErrorState error={revokeKey.error} />
        </div>
      ) : null}
    </>
  );
}
