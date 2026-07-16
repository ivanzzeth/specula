import { NavLink, Outlet } from 'react-router-dom';

import { useAuth } from '@/components/auth';
import { OrgSwitcher } from '@/components/org-switcher';
import { Toaster } from '@/components/ui/toaster';
import { cn } from '@/lib/utils';

interface NavItem {
  to: string;
  label: string;
  /** Requires system_role=="admin" (the cache/ops zone is cross-tenant). */
  adminOnly?: boolean;
  end?: boolean;
}

/**
 * The three zones of the app (REGISTRY-DESIGN §5).
 *
 * The grouping is the information architecture, and it answers the three
 * questions the design says users actually arrive with:
 *
 *   REGISTRY — "push/pull my private images"        → org → repo → tag
 *   CACHE    — "what has this proxy cached, and was it verified?"
 *   OPS      — "who's healthy, who has access, what's configured?"
 *
 * A zone is a visual group in the nav rail with a hairline between groups —
 * not a dropdown, not a sidebar tree. An operator sees every destination at
 * once; that is the instrument-panel read.
 */
const ZONES: { zone: string; items: NavItem[] }[] = [
  {
    zone: 'Registry',
    items: [
      { to: '/repos', label: 'Repositories' },
      { to: '/push', label: 'Push' },
    ],
  },
  {
    zone: 'Cache',
    items: [
      { to: '/', label: 'Overview', end: true },
      { to: '/cache', label: 'Browser', adminOnly: true },
      { to: '/events', label: 'Events', adminOnly: true },
    ],
  },
  {
    zone: 'Ops',
    items: [
      { to: '/upstreams', label: 'Upstreams', adminOnly: true },
      { to: '/members', label: 'Members' },
      { to: '/tokens', label: 'Tokens' },
      { to: '/users', label: 'Users', adminOnly: true },
      { to: '/config', label: 'Config', adminOnly: true },
    ],
  },
];

/**
 * Layout — the app shell.
 *
 * Two rails: a brand/identity bar and a nav bar. Both are hairline-separated
 * and dense (36px/32px). Nav state is TEXT COLOUR — active is amber, with no
 * pill, no filled background — which is the same state language the tabs and
 * sortable table headers use.
 */
export function Layout() {
  const { user, isAdmin, logout } = useAuth();

  return (
    <div className="flex h-full flex-col bg-slate-950">
      {/* ── identity rail ──────────────────────────────────────────────────── */}
      <header className="flex h-9 shrink-0 items-center gap-3 border-b border-slate-800 px-3">
        <div className="mr-1 flex items-center gap-2">
          {/* The one amber mark in the chrome: a square, not a rounded blob. */}
          <span className="grid size-4 place-items-center rounded-[2px] bg-brand text-[10px] font-bold text-brand-fg">
            S
          </span>
          <span className="text-data font-semibold tracking-tight text-slate-100">specula</span>
        </div>

        <OrgSwitcher />

        <div className="flex-1" />

        <div className="flex items-center gap-2.5 text-data text-slate-400">
          <span className="hidden max-w-[180px] truncate sm:block">{user?.email}</span>
          {isAdmin && (
            <span
              className="rounded-[2px] border border-brand/40 px-1 py-px text-micro font-semibold uppercase tracking-wider text-brand"
              title="System administrator: cross-org access to cache and ops."
            >
              admin
            </span>
          )}
          <button
            onClick={() => void logout()}
            className="text-slate-500 transition-colors duration-fast hover:text-slate-200 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
          >
            Sign out
          </button>
        </div>
      </header>

      {/* ── nav rail: three zones, hairline-separated ──────────────────────── */}
      <nav className="flex h-8 shrink-0 items-center gap-4 overflow-x-auto border-b border-slate-800 px-3">
        {ZONES.map((group, gi) => {
          const items = group.items.filter((i) => !i.adminOnly || isAdmin);
          if (items.length === 0) return null;
          return (
            <div key={group.zone} className="flex items-center gap-3">
              {gi > 0 && <span aria-hidden className="h-3.5 w-px bg-slate-800" />}
              <span className="text-micro font-semibold uppercase tracking-wider text-slate-600">
                {group.zone}
              </span>
              {items.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  className={({ isActive }) =>
                    cn(
                      'whitespace-nowrap text-data transition-colors duration-fast',
                      'focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring',
                      isActive
                        ? 'font-medium text-brand'
                        : 'text-slate-400 hover:text-slate-100'
                    )
                  }
                >
                  {item.label}
                </NavLink>
              ))}
            </div>
          );
        })}
      </nav>

      <main className="flex-1 overflow-y-auto p-4">
        <Outlet />
      </main>

      <Toaster />
    </div>
  );
}
