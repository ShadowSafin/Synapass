import { cookies } from 'next/headers';
import { NextResponse } from 'next/server';

import { GATEWAY_URL, verifySession } from '@/lib/gateway';

/**
 * Public client configuration.
 *
 * The gateway base URLs are not secrets (clients must know them to call the
 * API), so they are safe to expose to the browser. GATEWAY_LAN_URL is the
 * operator-configured LAN address (e.g. http://192.168.1.20:18080) shown
 * alongside localhost so anyone on the network can use an endpoint.
 * gateway_port is the public gateway port (parsed from GATEWAY_LAN_URL, else
 * from the public browser URL, else 18080) so the browser can derive the
 * current LAN address from the hostname it used to load the dashboard — a
 * fixed env IP goes stale every time the machine changes networks. The admin
 * key never leaves the server: only URLs are returned.
 *
 * The LAN address is disclosed only to a signed-in operator. The login and
 * setup pages never need it, and its only value to an anonymous caller is
 * mapping the internal network the gateway lives on.
 */
export async function GET() {
  const lanUrl = process.env.GATEWAY_LAN_URL ?? '';

  const jar = await cookies();
  const cookie = jar
    .getAll()
    .map((entry) => `${entry.name}=${entry.value}`)
    .join('; ');
  const authenticated = await verifySession(cookie);

  return NextResponse.json({
    gateway_url: GATEWAY_URL,
    gateway_port: publicGatewayPort(lanUrl),
    ...(authenticated ? { lan_url: lanUrl } : {}),
  });
}

/** Best-effort public gateway port for LAN derivation; never throws. */
function publicGatewayPort(lanUrl: string): number {
  for (const raw of [lanUrl, process.env.NEXT_PUBLIC_SYNAPASS_API_URL ?? '']) {
    if (!raw) continue;
    try {
      const port = new URL(raw).port;
      if (port) return Number(port);
    } catch {
      // A bare hostname or service name carries no port; try the next source.
    }
  }
  return 18080;
}
