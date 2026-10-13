import { afterEach, describe, expect, it, vi } from 'vitest';

import { ApiError, apiFetch, buildQuery, toCsv } from './api';

describe('ApiError', () => {
  it('carries the gateway message, status and envelope codes', () => {
    const error = new ApiError('rate limited', 429, 'rate_limit_error', 'rate_limit_error');
    expect(error).toBeInstanceOf(Error);
    expect(error.name).toBe('ApiError');
    expect(error.message).toBe('rate limited');
    expect(error.status).toBe(429);
    expect(error.code).toBe('rate_limit_error');
    expect(error.type).toBe('rate_limit_error');
  });

  it('flags dashboard misconfiguration separately from bad requests', () => {
    expect(new ApiError('missing', 500, 'dashboard_missing_admin_key').isConfigurationError).toBe(true);
    expect(new ApiError('unreachable', 502, 'dashboard_upstream_unreachable').isConfigurationError).toBe(
      true,
    );
    expect(new ApiError('rate limited', 429, 'rate_limit_error').isConfigurationError).toBe(false);
    expect(new ApiError('plain', 500).isConfigurationError).toBe(false);
  });

  it('flags unauthorized statuses', () => {
    expect(new ApiError('x', 401).isUnauthorized).toBe(true);
    expect(new ApiError('x', 403).isUnauthorized).toBe(true);
    expect(new ApiError('x', 404).isUnauthorized).toBe(false);
    expect(new ApiError('x', 0, 'dashboard_offline').isUnauthorized).toBe(false);
  });
});

describe('buildQuery', () => {
  it('serializes plain params with a leading question mark', () => {
    expect(buildQuery({ tenant_id: 'acme', from: '24h' })).toBe('?tenant_id=acme&from=24h');
  });

  it('drops empty values so the gateway sees clean input', () => {
    expect(buildQuery({ tenant_id: '', from: undefined, limit: null })).toBe('');
    expect(buildQuery({ outcome: '', provider: 'openai' })).toBe('?provider=openai');
  });

  it('keeps zero and numeric values', () => {
    expect(buildQuery({ limit: 0, offset: 50 })).toBe('?limit=0&offset=50');
  });

  it('encodes values that need it', () => {
    // URLSearchParams renders a space as `+`.
    expect(buildQuery({ search: 'req 1&2' })).toBe('?search=req+1%262');
  });

  it('returns an empty string when nothing survives', () => {
    expect(buildQuery({})).toBe('');
  });
});

describe('toCsv', () => {
  const columns = ['created_at', 'request_id', 'outcome', 'error_message'];

  it('writes the header when there are no rows', () => {
    expect(toCsv([], columns)).toBe('created_at,request_id,outcome,error_message');
  });

  it('serializes rows in column order', () => {
    const csv = toCsv(
      [{ created_at: '2026-10-01', request_id: 'req_1', outcome: 'success', error_message: '' }],
      columns,
    );
    expect(csv).toBe('created_at,request_id,outcome,error_message\n2026-10-01,req_1,success,');
  });

  it('renders null and undefined cells as empty', () => {
    const csv = toCsv(
      [{ created_at: null, request_id: undefined, outcome: 'success', error_message: null }],
      columns,
    );
    expect(csv).toBe('created_at,request_id,outcome,error_message\n,,success,');
  });

  it('renders a missing column as empty rather than "undefined"', () => {
    const csv = toCsv([{ request_id: 'req_1' }], ['request_id', 'provider']);
    expect(csv).toBe('request_id,provider\nreq_1,');
  });

  it('quotes cells containing commas, quotes or newlines', () => {
    const csv = toCsv(
      [
        {
          created_at: '2026-10-01',
          request_id: 'req_1',
          outcome: 'error',
          error_message: 'has "quotes", commas\nand a newline',
        },
      ],
      columns,
    );
    expect(csv).toContain('"has ""quotes"", commas\nand a newline"');
  });

  it('round-trips the request-log export shape', () => {
    const exportColumns = [
      'created_at',
      'request_id',
      'tenant_id',
      'requested_model',
      'routed_model',
      'provider',
      'strategy',
      'status',
      'outcome',
      'error_code',
      'latency_ms',
      'attempts',
      'fallback_used',
      'prompt_tokens',
      'completion_tokens',
      'cost_usd',
    ];
    const csv = toCsv(
      [
        {
          created_at: '2026-10-01T00:00:00Z',
          request_id: 'req_1',
          tenant_id: 'acme',
          requested_model: 'gpt-4o-mini',
          routed_model: 'gpt-4o-mini',
          provider: 'openai',
          strategy: 'priority',
          status: 200,
          outcome: 'success',
          error_code: '',
          latency_ms: 812,
          attempts: 1,
          fallback_used: false,
          prompt_tokens: 10,
          completion_tokens: 3,
          cost_usd: 0.0001,
        },
      ],
      exportColumns,
    );
    const [header, body] = csv.split('\n');
    expect(header).toBe(exportColumns.join(','));
    expect(body?.split(',')).toHaveLength(exportColumns.length);
  });
});

describe('apiFetch', () => {
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('calls the same-origin proxy with JSON accept and no credential', async () => {
    const seen: { url?: string; init?: RequestInit } = {};
    vi.stubGlobal(
      'fetch',
      vi.fn(async (url: string, init?: RequestInit) => {
        seen.url = url;
        seen.init = init;
        return new Response(JSON.stringify({ ok: true }), { status: 200 });
      }),
    );

    const payload = await apiFetch<{ ok: boolean }>('usage/summary?from=24h');
    expect(payload).toEqual({ ok: true });
    expect(seen.url).toBe('/api/gateway/usage/summary?from=24h');
    expect(seen.init?.headers).toMatchObject({ accept: 'application/json' });
    // The admin credential is attached server-side, never from the browser.
    expect(JSON.stringify(seen.init?.headers)).not.toContain('Bearer');
  });

  it('serializes a body as JSON with the right content type', async () => {
    let body: string | undefined;
    let contentType: string | null = null;
    vi.stubGlobal(
      'fetch',
      vi.fn(async (_url: string, init?: RequestInit) => {
        body = init?.body as string;
        contentType = new Headers(init?.headers).get('content-type');
        return new Response(JSON.stringify({ ok: true }), { status: 200 });
      }),
    );

    await apiFetch('providers', { method: 'POST', body: { name: 'openai' } });
    expect(contentType).toBe('application/json');
    expect(JSON.parse(body ?? '')).toEqual({ name: 'openai' });
  });

  it('resolves undefined for empty bodies and 204 responses', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response(null, { status: 204 })));
    await expect(apiFetch('keys/key_1', { method: 'DELETE' })).resolves.toBeUndefined();

    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 200 })));
    await expect(apiFetch('overview')).resolves.toBeUndefined();
  });

  it('throws the gateway envelope message with its status', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({ error: { message: 'rate limited', code: 'rate_limit_error' } }),
            { status: 429 },
          ),
      ),
    );

    const failure = await apiFetch('usage/summary').catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(ApiError);
    expect(failure).toMatchObject({ status: 429, message: 'rate limited', code: 'rate_limit_error' });
  });

  it('falls back to the status description when the error body is not JSON', async () => {
    vi.stubGlobal('fetch', vi.fn(async () => new Response('', { status: 404 })));
    const failure = await apiFetch('providers/nope').catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(ApiError);
    expect((failure as ApiError).message).toBe('not found');
  });

  it('reports a down dashboard server as dashboard_offline, not a gateway error', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => {
        throw new Error('fetch failed');
      }),
    );
    const failure = await apiFetch('overview').catch((error: unknown) => error);
    expect(failure).toBeInstanceOf(ApiError);
    expect(failure).toMatchObject({ status: 0, code: 'dashboard_offline' });
  });
});
