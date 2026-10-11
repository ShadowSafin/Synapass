# Synapass Dashboard

The control plane's operator console: what the gateway served, how it routed it,
what it cost and whether the providers are healthy.

## Why it proxies rather than calling the gateway directly

Every administrative read goes through `src/app/api/gateway/[...path]/route.ts`,
a Next.js route handler that attaches the admin credential server-side. The
browser never holds it.

That is not a stylistic choice. The gateway's `/admin/v1` surface requires a
credential with an admin scope, so shipping one in the browser bundle would make
every visitor a platform operator. The proxy also means the gateway's CORS policy
does not have to be widened to expose administrative routes.

```text
browser ──/api/gateway/overview──▶ Next server ──Authorization: Bearer …──▶ gateway /admin/v1/overview
```

The route only ever targets `<gateway>/admin/v1/<path>`, so it cannot be turned
into a general-purpose forwarder for the gateway.

## Configuration

| Variable | Default | Purpose |
| --- | --- | --- |
| `SYNAPASS_API_URL` | `http://localhost:8080` | The gateway as seen from the dashboard server. |
| `SYNAPASS_ADMIN_KEY` | *(empty)* | Admin credential. The gateway reads the same variable, so one value covers both. |

With `SYNAPASS_ADMIN_KEY` unset the dashboard still renders, and every page
shows a configuration error that says exactly which variable to set. A silent
401 is the failure mode this avoids.

`NEXT_PUBLIC_SYNAPASS_API_URL` is only needed if a panel links directly to the
gateway (for example the `/metrics` endpoint); it is inlined at build time.

## Development

```bash
cd dashboard
npm install
SYNAPASS_API_URL=http://localhost:8080 \
SYNAPASS_ADMIN_KEY=<the gateway's admin key> \
npm run dev
```

Then open <http://localhost:3000>. `npm run dev` binds `0.0.0.0`, so other
devices on the same network can use `http://<your-lan-ip>:3000` too (allow
Node.js through the OS firewall on first prompt). The dev server
auto-allowlists your current LAN IPs (`allowedDevOrigins` in
`next.config.mjs`); add more names with
`NEXT_ALLOWED_DEV_ORIGINS="my-pc.local,192.168.1.20"`.

## Build

```bash
npm ci
npm run build      # emits .next/standalone, which the container image uses
npm run typecheck  # tsc --noEmit, no bundle emitted
```

## Layout

```text
src/
  app/
    api/gateway/[...path]/route.ts   server-side admin proxy
    <route>/page.tsx                 route shells; each wraps a view in Suspense
    layout.tsx  providers.tsx  globals.css
  components/
    views/       one client component per page, owning its data fetching
    ui/          shadcn-style primitives (button, card, badge, table, …)
    charts/      dependency-free SVG line chart and bar list
    layout/      sidebar and shell
  hooks/use-admin.ts   every React Query hook, with one query-key factory
  lib/
    api.ts       browser client for the proxy
    gateway.ts   server-only gateway client (never imported by a client component)
    types.ts     hand-written mirrors of the gateway's admin payloads
    format.ts    every number an operator reads, formatted in one place
    metrics.ts   derived figures (success rate, deltas, bucket labels)
```

## Charts

There is no charting library. Both charts are a few polylines over a numeric
array, which the SVG components render identically on the server and the client.
That is a smaller bundle, one fewer upgrade to track, and no canvas-vs-SSR
mismatch.

## Development notes

- **Add a page** by adding a `views/*-view.tsx` client component and a
  `app/<route>/page.tsx` shell. Any view that reads the query string must be
  wrapped in `<Suspense>`; the shells already do this.
- **Add an endpoint** by adding a hook to `hooks/use-admin.ts` and a key to the
  `queryKeys` factory. Invalidate through the factory rather than with a
  hand-written array, or the invalidation will silently miss.
- **Time windows** live in the URL (`?window=24h`), because the gateway parses
  `from` with Go's `time.ParseDuration`, which has no day unit — `7d` is sent as
  `168h`.
