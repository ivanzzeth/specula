import { NavLink, Outlet } from 'react-router-dom';
import { useTranslation } from 'react-i18next';

import { useAuth } from '@/components/auth';
import { LanguageSwitcher } from '@/components/language-switcher';
import { OrgSwitcher } from '@/components/org-switcher';
import { Toaster } from '@/components/ui/toaster';
import { cn } from '@/lib/utils';

interface NavItem {
  to: string;
  /** i18n key under `nav.*`. Resolved at render, not at module scope, so a
   *  language switch re-renders the rail instead of stranding it. */
  labelKey: string;
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
const ZONES: { zoneKey: string; items: NavItem[] }[] = [
  {
    zoneKey: 'nav.zones.registry',
    items: [
      { to: '/repos', labelKey: 'nav.repos' },
      { to: '/push', labelKey: 'nav.push' },
    ],
  },
  {
    zoneKey: 'nav.zones.cache',
    items: [
      { to: '/', labelKey: 'nav.overview', end: true },
      { to: '/cache', labelKey: 'nav.browser', adminOnly: true },
      { to: '/events', labelKey: 'nav.events', adminOnly: true },
    ],
  },
  {
    zoneKey: 'nav.zones.ops',
    items: [
      { to: '/upstreams', labelKey: 'nav.upstreams', adminOnly: true },
      { to: '/members', labelKey: 'nav.members' },
      { to: '/tokens', labelKey: 'nav.tokens' },
      { to: '/users', labelKey: 'nav.users', adminOnly: true },
      { to: '/config', labelKey: 'nav.config', adminOnly: true },
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
  const { t } = useTranslation();

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
              className="label-caps rounded-[2px] border border-brand/40 px-1 py-px text-micro font-semibold text-brand"
              title={t('nav.adminTitle')}
            >
              {t('nav.admin')}
            </span>
          )}
          <LanguageSwitcher />
          <button
            onClick={() => void logout()}
            className="whitespace-nowrap text-slate-500 transition-colors duration-fast hover:text-slate-200 focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring"
          >
            {t('nav.signOut')}
          </button>
        </div>
      </header>

      {/* ── nav rail: three zones, hairline-separated ──────────────────────── */}
      <nav className="flex h-8 shrink-0 items-center gap-4 overflow-x-auto border-b border-slate-800 px-3">
        {ZONES.map((group, gi) => {
          const items = group.items.filter((i) => !i.adminOnly || isAdmin);
          if (items.length === 0) return null;
          return (
            <div key={group.zoneKey} className="flex items-center gap-3">
              {gi > 0 && <span aria-hidden className="h-3.5 w-px bg-slate-800" />}
              <span className="label-caps text-micro font-semibold text-slate-600">
                {t(group.zoneKey)}
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
                  {t(item.labelKey)}
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
