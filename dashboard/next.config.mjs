/**
 * Next.js configuration.
 *
 * `output: 'standalone'` is what the production image depends on: it emits a
 * self-contained server bundle with only the modules actually imported, so the
 * runtime image does not need node_modules. See deploy/docker/Dockerfile.dashboard.
 *
 * `outputFileTracingRoot` pins tracing to this directory. Without it, a
 * checkout nested under another lockfile (like this repository) makes Next
 * infer the workspace root above the dashboard, which nests the standalone
 * output one level deeper and breaks every launcher that expects
 * `.next/standalone/server.js` — the container build, `synapass native up`,
 * and the systemd unit alike.
 *
 * @type {import('next').NextConfig}
 */
import path from 'node:path';
import os from 'node:os';
import { fileURLToPath } from 'node:url';

const dashboardDir = path.dirname(fileURLToPath(import.meta.url));

/**
 * Collect the machine's current LAN IPv4 addresses so `next dev` (which binds
 * 0.0.0.0 via `npm run dev`) accepts dev requests when the dashboard is opened
 * from another device on the same network (e.g. http://192.168.1.20:3000).
 * Without this, Next blocks the cross-origin dev request and tells you to
 * configure `allowedDevOrigins`.
 */
function localLanHosts() {
  const hosts = new Set();
  for (const addrs of Object.values(os.networkInterfaces())) {
    for (const a of addrs ?? []) {
      if (a.family === 'IPv4' && !a.internal && a.address) {
        hosts.add(a.address);
      }
    }
  }
  return [...hosts];
}

const envAllowedDevOrigins = (process.env.NEXT_ALLOWED_DEV_ORIGINS ?? '')
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean);

const nextConfig = {
  output: 'standalone',
  outputFileTracingRoot: dashboardDir,
  reactStrictMode: true,
  // LAN-first dev: `npm run dev` binds 0.0.0.0, so phones and other devices on
  // the same network can open the dashboard via this machine's LAN IP.
  // Next blocks dev requests whose Origin is not allowlisted, so the current
  // LAN addresses are allowlisted automatically. Pin extra names with
  // NEXT_ALLOWED_DEV_ORIGINS="my-pc.local,192.168.1.20".
  allowedDevOrigins: [...localLanHosts(), ...envAllowedDevOrigins],
  // The framework version is not an interesting disclosure.
  poweredByHeader: false,
  eslint: {
    // Linting is a separate concern from building; a lint rule should not be able
    // to fail a container image build.
    ignoreDuringBuilds: true,
  },
  async headers() {
    return [
      {
        source: '/:path*',
        headers: [
          // The dashboard renders operator-supplied values (model names, key
          // names, error text) into the DOM, so a strict content policy and
          // framing denial are appropriate even though the surface is internal.
          { key: 'X-Content-Type-Options', value: 'nosniff' },
          { key: 'X-Frame-Options', value: 'DENY' },
          { key: 'Referrer-Policy', value: 'no-referrer' },
        ],
      },
    ];
  },
};

export default nextConfig;
