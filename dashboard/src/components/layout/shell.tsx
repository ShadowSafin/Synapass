'use client';

import { Menu, X } from 'lucide-react';
import { usePathname } from 'next/navigation';
import * as React from 'react';

import { cn } from '@/lib/utils';

import { Sidebar, SidebarBrand } from './sidebar';

/** Routes that render their own full-screen layout, with no rail or footer. */
const BARE_PATHS = new Set(['/login', '/setup']);

/**
 * Routes that paint the whole content column edge to edge, with no max-width
 * and no padding — the page owns its own spacing. Used by full-bleed canvases
 * (the Playground gradient); on wide monitors a centered max-width would leave
 * black bands on both sides.
 */
const WIDE_PATHS = new Set(['/playground']);

/**
 * A little of the Playground sunset, for the chrome around it.
 *
 * These washes sit behind the rail, drawer and footer on the Playground route
 * only, so the dark chrome picks up a hint of the page gradient without
 * changing legibility — the surfaces stay dark, the text untouched. The
 * hairlines carry the same hues at full strength along the chrome edges.
 */
const RAIL_WASH =
  'bg-[linear-gradient(180deg,rgba(168,196,244,0.15),rgba(188,174,240,0.12)_26%,rgba(217,169,230,0.10)_44%,rgba(240,166,214,0.11)_60%,rgba(249,189,140,0.13)_78%,rgba(246,156,82,0.16))]';
const BAR_WASH =
  'bg-[linear-gradient(90deg,rgba(168,196,244,0.10),rgba(188,174,240,0.09)_25%,rgba(217,169,230,0.10)_45%,rgba(240,166,214,0.12)_65%,rgba(249,189,140,0.14))]';
const EDGE_VERTICAL =
  'bg-[linear-gradient(180deg,rgba(168,196,244,0.55),rgba(217,169,230,0.45)_35%,rgba(240,166,214,0.5)_60%,rgba(246,156,82,0.65))]';
const EDGE_HORIZONTAL =
  'bg-[linear-gradient(90deg,rgba(168,196,244,0.5),rgba(217,169,230,0.4)_30%,rgba(240,166,214,0.45)_60%,rgba(246,156,82,0.6))]';

/**
 * The application shell: a full-screen black canvas with a fixed sidebar and
 * a scrolling content column.
 *
 * No boxed container, no floating panel — the rail and the content sit
 * directly on the page background, separated by a single hairline. On narrow
 * screens the rail becomes a drawer. The one authored motion in the product
 * lives here: the drawer slide. Everything else stays still.
 */
export function Shell({ children }: { children: React.ReactNode }) {
  const pathname = usePathname();
  const [navOpen, setNavOpen] = React.useState(false);
  const menuButtonRef = React.useRef<HTMLButtonElement>(null);

  const closeNav = React.useCallback(() => {
    setNavOpen(false);
    menuButtonRef.current?.focus();
  }, []);

  React.useEffect(() => {
    if (!navOpen) return;
    const onKey = (event: KeyboardEvent) => {
      if (event.key === 'Escape') closeNav();
    };
    document.addEventListener('keydown', onKey);
    document.body.style.overflow = 'hidden';
    return () => {
      document.removeEventListener('keydown', onKey);
      document.body.style.overflow = '';
    };
  }, [navOpen, closeNav]);

  // The auth screens are the one place the application chrome would be noise:
  // there is no session to navigate with yet, so render them edge to edge.
  if (BARE_PATHS.has(pathname)) {
    return <>{children}</>;
  }

  // The Playground paints its own full-bleed canvas; the surrounding chrome
  // picks up a little of its gradient so the page feels like one surface.
  const playground = WIDE_PATHS.has(pathname);

  return (
    <div className="min-h-screen overflow-x-clip bg-background">
      {/* Mobile top bar. The sidebar owns navigation on desktop; this bar only
          exists to open it where the rail does not fit. */}
      <div
        className={cn(
          'sticky top-0 z-30 flex h-14 items-center gap-3 border-b border-border bg-background px-4 lg:hidden',
          playground && BAR_WASH,
        )}
      >
        <button
          ref={menuButtonRef}
          type="button"
          onClick={() => setNavOpen(true)}
          aria-label="Open navigation"
          aria-expanded={navOpen}
          className="flex size-9 items-center justify-center rounded-lg border border-border bg-card text-muted-foreground transition-colors hover:text-foreground"
        >
          <Menu className="size-4" />
        </button>
        <SidebarBrand compact />
      </div>

      <div className="flex min-h-screen min-w-0 lg:min-h-screen">
        {/* Desktop rail. Transparent sidebar, so the wash behind it tints the nav. */}
        <div
          className={cn(
            'relative hidden shrink-0 border-r lg:block',
            playground ? 'border-transparent' : 'border-border',
            playground && RAIL_WASH,
          )}
        >
          {playground ? (
            <div aria-hidden className={cn('absolute inset-y-0 right-0 w-px', EDGE_VERTICAL)} />
          ) : null}
          <Sidebar />
        </div>

        {/* Mobile drawer. */}
        <div
          className={`fixed inset-0 z-40 lg:hidden ${navOpen ? '' : 'pointer-events-none'}`}
          aria-hidden={!navOpen}
        >
          <div
            className={`absolute inset-0 bg-black/70 transition-opacity duration-200 ease-out ${
              navOpen ? 'opacity-100' : 'opacity-0'
            }`}
            onClick={closeNav}
          />
          <div
            role="dialog"
            aria-modal="true"
            aria-label="Navigation"
            className={cn(
              `absolute inset-y-0 left-0 w-[min(280px,85vw)] border-r bg-card transition-transform duration-200 ease-out ${
                navOpen ? 'translate-x-0' : '-translate-x-full'
              }`,
              playground ? 'border-transparent' : 'border-border',
              playground && RAIL_WASH,
            )}
          >
            {playground ? (
              <div aria-hidden className={cn('absolute inset-y-0 right-0 w-px', EDGE_VERTICAL)} />
            ) : null}
            <div className="flex h-14 items-center justify-between border-b border-border px-4">
              <SidebarBrand compact />
              <button
                type="button"
                onClick={closeNav}
                aria-label="Close navigation"
                className="flex size-9 items-center justify-center rounded-lg border border-border text-muted-foreground transition-colors hover:text-foreground"
              >
                <X className="size-4" />
              </button>
            </div>
            <Sidebar onNavigate={closeNav} inDrawer />
          </div>
        </div>

        <div className="flex min-w-0 flex-1 flex-col overflow-x-clip">
          <main
            className={
              WIDE_PATHS.has(pathname)
                ? 'w-full min-w-0 flex-1'
                : 'mx-auto w-full min-w-0 max-w-[1500px] flex-1 overflow-x-clip px-3 py-4 sm:px-6 sm:py-8 lg:px-10'
            }
          >
            {children}
          </main>
          <footer
            className={cn(
              'relative border-t px-4 py-3 text-center text-[11px] leading-relaxed text-muted-foreground',
              playground ? 'border-transparent' : 'border-border',
              playground && BAR_WASH,
            )}
          >
            {playground ? (
              <div aria-hidden className={cn('absolute inset-x-0 top-0 h-px', EDGE_HORIZONTAL)} />
            ) : null}
            Synapass control plane · administrative views are read-only unless a button offers an action
          </footer>
        </div>
      </div>
    </div>
  );
}
