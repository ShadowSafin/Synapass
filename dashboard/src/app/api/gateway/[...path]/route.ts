/**
 * Same-origin proxy to the gateway's admin API.
 *
 * Why a proxy rather than calling the gateway from the browser:
 *
 *   1. The admin API requires a credential. Shipping it in the browser bundle
 *      would make every visitor a platform operator.
 *   2. The gateway's admin routes are not CORS-enabled for the dashboard origin
 *      by default, and widening CORS to expose them would be worse.
 *   3. Inside docker compose the browser can reach the gateway on localhost but
 *      the dashboard server reaches it faster over the compose network.
 *
 * The route only ever targets `<gateway>/admin/v1/<path>`, so it cannot be
 * turned into a general-purpose forwarder for the gateway.
 *
 * ## Authentication
 *
 * The proxy refuses to attach the dashboard's admin key unless the gateway has
 * confirmed the operator's session. Without this check the proxy is an open
 * administrative backdoor: the browser never needs to know the key, only to be
 * able to reach this route, which makes "can reach the dashboard" equivalent to
 * "is an operator".
 *
 * The check is a real round trip rather than a cookie-presence test. An expired,
 * revoked or forged cookie must not be enough, and only the gateway can tell.
 *
 * ## How the session is presented
 *
 * The operator's `Cookie` header is forwarded verbatim on the outbound call, so
 * the gateway sees exactly the same cookie the browser sent and keeps a single way
 * to accept a session. Inventing a second, header-based way in would mean two
 * code paths protecting the same thing, and one of them would eventually drift.
 */
import { NextResponse } from 'next/server';

import { hasAdminKey, proxyAdmin, verifySession } from '@/lib/gateway';

export const dynamic = 'force-dynamic';
// Proxying is a pass-through, not a cache.
export const revalidate = 0;

interface RouteContext {
  params: Promise<{ path: string[] }>;
}

/** Methods that cannot change state and therefore do not need CSRF protection. */
const SAFE_METHODS = new Set(['GET', 'HEAD', 'OPTIONS']);

/**
 * Reject state-changing requests that did not come from this console's origin.
 *
 * The session cookie is SameSite=Lax, which stops a cross-site form post but
 * does not stop a request from another origin on the same site — a different
 * port on 127.0.0.1 (a dev server, another local app) or on the same LAN host.
 * That is enough to drive this proxy with the operator's live session, so the
 * browser's own statement of origin is checked as well.
 *
 * A missing Origin is accepted because non-browser clients do not send one and
 * cannot be CSRF'd; a browser always sends it on a state-changing request.
 */
function isSameOriginMutation(request: Request): boolean {
  const site = request.headers.get('sec-fetch-site');
  if (site && site !== 'same-origin' && site !== 'none') {
    return false;
  }
  const origin = request.headers.get('origin');
  // No Origin: a non-browser client (curl, server-side fetch). Browsers always
  // send it on a state-changing request, and a client that cannot be CSRF'd
  // does not need this check.
  if (!origin) return true;

  // Compare against the Host header, not request.url: the standalone Next
  // server builds request.url from its listen address (0.0.0.0 inside the
  // container), so an exact URL comparison would reject every real browser.
  const host = request.headers.get('host');
  if (!host) return false;
  try {
    return new URL(origin).host === host;
  } catch {
    return false;
  }
}

async function handle(request: Request, context: RouteContext): Promise<NextResponse> {
  const { path } = await context.params;
  const route = path.join('/');
  const method = request.method.toUpperCase();

  if (!SAFE_METHODS.has(method) && !isSameOriginMutation(request)) {
    return NextResponse.json(
      {
        error: {
          message: 'cross-origin requests may not change administrative state',
          type: 'permission_error',
          code: 'cross_origin_rejected',
        },
      },
      { status: 403, headers: { 'cache-control': 'no-store' } },
    );
  }

  if (!hasAdminKey()) {
    // Fail with an actionable message rather than a 401 from the gateway, which
    // reads as a credentials problem when it is a configuration one.
    return NextResponse.json(
      {
        error: {
          message:
            'the dashboard has no administrative credential configured. ' +
            'Set SYNAPASS_ADMIN_KEY to the admin key the gateway accepts.',
          type: 'configuration_error',
          code: 'dashboard_missing_admin_key',
        },
      },
      { status: 503 },
    );
  }

  const cookie = request.headers.get('cookie') ?? '';

  // The auth endpoints are how an operator obtains a session, so they cannot
  // require one. They are safe to reach unauthenticated because the gateway
  // validates them itself: setup is latched shut after the first operator,
  // login is rate limited and locked out, logout is idempotent, and state
  // discloses nothing beyond whether setup is pending.
  if (!route.startsWith('auth/')) {
    if (!(await verifySession(cookie))) {
      return NextResponse.json(
        {
          error: {
            message: 'authentication is required',
            type: 'authentication_error',
            code: 'not_authenticated',
          },
        },
        { status: 401 },
      );
    }
  }

  const search = new URL(request.url).search;
  const target = `${route}${search}`;  const body = method === 'GET' || method === 'HEAD' ? undefined : await request.text();

  const result = await proxyAdmin(method, target, {
    body,
    contentType: request.headers.get('content-type') ?? undefined,
    signal: request.signal,
    cookie,
  });

  const headers: Record<string, string> = { 'cache-control': 'no-store' };
  // Only when there is one: a 204 carries no content type, and an empty header
  // value is rejected outright, which turned a successful logout into a 500.
  if (result.contentType) {
    headers['content-type'] = result.contentType;
  }

  // Set-Cookie must be forwarded verbatim. The gateway is what issues the
  // session cookie, and dropping the header here would mean a successful login
  // left the browser holding nothing: the response says 200 and the next request
  // is unauthenticated again.
  if (result.setCookie) {
    headers['set-cookie'] = result.setCookie;
  }

  // 204 and 304 must carry a null body; passing an empty string is rejected by
  // the Response constructor, which turned a correct logout into a 500.
  const bodyless = result.status === 204 || result.status === 304;
  return new NextResponse(bodyless ? null : result.body, { status: result.status, headers });
}

export const GET = handle;
export const POST = handle;
export const PUT = handle;
export const PATCH = handle;
export const DELETE = handle;
