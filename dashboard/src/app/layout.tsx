import type { Metadata, Viewport } from 'next';
import { Geist, Geist_Mono, Instrument_Serif } from 'next/font/google';

import { Shell } from '@/components/layout/shell';

import './globals.css';
import { Providers } from './providers';

/**
 * Geist carries the whole product voice: UI text and tabular figures. It is
 * bundled at build time, so the dashboard renders identically offline and
 * behind air-gapped proxies — no external font request at runtime.
 *
 * Geist Mono handles ids, hashes and code — monospace is reserved for data,
 * never used as decoration.
 *
 * Instrument Serif is the display face, reserved for the handful of large
 * headlines (the auth screens' serif headings) where a little editorial
 * contrast belongs. Body and data stay in the grotesque.
 */
const geistSans = Geist({
  subsets: ['latin'],
  display: 'swap',
  variable: '--font-sans',
});

const geistMono = Geist_Mono({
  subsets: ['latin'],
  display: 'swap',
  variable: '--font-mono',
});

const instrumentSerif = Instrument_Serif({
  subsets: ['latin'],
  weight: '400',
  style: ['normal', 'italic'],
  display: 'swap',
  variable: '--font-serif',
});

export const metadata: Metadata = {
  title: {
    default: 'Synapass',
    template: '%s · Synapass',
  },
  description: 'Synapass control plane: traffic, routing, providers and spend.',
  robots: { index: false, follow: false },
};

export const viewport: Viewport = {
  width: 'device-width',
  initialScale: 1,
  viewportFit: 'cover',
  themeColor: [
    { media: '(prefers-color-scheme: light)', color: '#ffffff' },
    { media: '(prefers-color-scheme: dark)', color: '#0b1220' },
  ],
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html
      lang="en"
      className={`dark ${geistSans.variable} ${geistMono.variable} ${instrumentSerif.variable}`}
    >
      <body className="font-sans">
        <Providers>
          <Shell>{children}</Shell>
        </Providers>
      </body>
    </html>
  );
}
