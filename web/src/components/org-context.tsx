import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from 'react';

import { getMe, listOrgs, setActiveOrg as setClientOrg } from '@/api/client';
import type { OrgDTO } from '@/api/types';

const STORAGE_KEY = 'specula:activeOrgId';

interface OrgCtx {
  /** Orgs the caller belongs to. Empty until loaded. */
  orgs: OrgDTO[];
  /** The active org, or null before load / when the caller has none. */
  activeOrg: OrgDTO | null;
  /** The caller's role in the active org ("viewer" | "editor" | "admin" | "owner"). */
  role: string;
  /** True when the caller may administer the active org (admin or owner). */
  canAdminOrg: boolean;
  loading: boolean;
  /** Switch orgs. Persists the choice and re-points the API client's X-Org-Id. */
  switchOrg: (orgId: string) => void;
  /** Re-fetch the org list (after creating an org, accepting an invite, …). */
  refresh: () => void;
}

const Ctx = createContext<OrgCtx>({
  orgs: [],
  activeOrg: null,
  role: '',
  canAdminOrg: false,
  loading: true,
  switchOrg: () => {},
  refresh: () => {},
});

/**
 * OrgProvider owns the multi-tenant context for the whole app.
 *
 * The active org is the single piece of ambient state every org-context request
 * depends on (X-Org-Id). It lives here, is pushed into the API client on every
 * change, and is persisted to localStorage so a reload does not silently drop
 * an operator into a different tenant than the one they were working in.
 *
 * The selection is validated against the caller's real org list on load: a
 * persisted id for an org they have since been removed from must not stick.
 *
 * Mount INSIDE AuthProvider — it only makes sense for an authenticated caller.
 */
export function OrgProvider({ children }: { children: ReactNode }) {
  const [orgs, setOrgs] = useState<OrgDTO[]>([]);
  const [activeOrgId, setActiveOrgId] = useState<string | null>(() => {
    const stored = localStorage.getItem(STORAGE_KEY);
    // Push the persisted choice into the client before the first request goes
    // out, so the initial page load is already scoped to the right org.
    if (stored) setClientOrg(stored);
    return stored;
  });
  const [role, setRole] = useState('');
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(() => {
    setLoading(true);
    Promise.all([listOrgs().catch(() => ({ orgs: [] })), getMe().catch(() => null)])
      .then(([orgList, me]) => {
        const list = orgList.orgs ?? [];
        setOrgs(list);

        setActiveOrgId((current) => {
          // Keep the current/persisted choice only if it is still a real
          // membership; otherwise fall back to the server's view, then to the
          // first org available.
          const valid = current && list.some((o) => o.id === current) ? current : null;
          const next = valid ?? me?.active_org_id ?? list[0]?.id ?? null;
          setClientOrg(next);
          if (next) localStorage.setItem(STORAGE_KEY, next);
          else localStorage.removeItem(STORAGE_KEY);
          return next;
        });

        if (me?.active_org_role) setRole(me.active_org_role);
      })
      .finally(() => setLoading(false));
  }, []);

  useEffect(refresh, [refresh]);

  const switchOrg = useCallback(
    (orgId: string) => {
      setActiveOrgId(orgId);
      setClientOrg(orgId);
      localStorage.setItem(STORAGE_KEY, orgId);
      // The role is per-org, so it is stale the moment the org changes. Clear
      // it and re-resolve rather than briefly showing the previous org's rights.
      setRole('');
      getMe()
        .then((me) => setRole(me.active_org_role ?? ''))
        .catch(() => setRole(''));
    },
    []
  );

  const value = useMemo<OrgCtx>(() => {
    const activeOrg = orgs.find((o) => o.id === activeOrgId) ?? null;
    // Prefer the role the org list reports for this org; fall back to /me's.
    const effectiveRole = activeOrg?.role || role;
    return {
      orgs,
      activeOrg,
      role: effectiveRole,
      canAdminOrg: effectiveRole === 'admin' || effectiveRole === 'owner',
      loading,
      switchOrg,
      refresh,
    };
  }, [orgs, activeOrgId, role, loading, switchOrg, refresh]);

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

/** useOrg reads the active-org context. */
export function useOrg(): OrgCtx {
  return useContext(Ctx);
}

/**
 * RequireOrg gates routes whose every request carries X-Org-Id (repos, tags,
 * push, members, tokens). Without a resolved org those pages cannot form a
 * single valid request.
 *
 * It exists because five pages were each hand-rolling this branch and drifting:
 * some rendered "No active organisation", one silently showed an empty table,
 * and two printed different placeholder org names into copy-paste-able shell
 * commands (`<org>` vs `{org}`) — a command a user could paste and have fail.
 * One guard, one answer, before the page mounts.
 *
 * Mount INSIDE OrgProvider. Pages keep their own defensive `!activeOrg` checks;
 * those are now unreachable fallbacks rather than the primary handling.
 */
export function RequireOrg({ children }: { children: ReactNode }) {
  const { activeOrg, orgs, loading } = useOrg();

  // Hold the layout while the org list resolves — flashing "no organisation"
  // at a user who has one would be a lie told for 200ms.
  if (loading) {
    return (
      <div className="rounded border border-slate-800 bg-slate-900 p-8 text-center">
        <span
          role="status"
          aria-label="Loading organisations"
          className="inline-block size-4 animate-spin rounded-full border-2 border-slate-800 border-t-brand"
        />
      </div>
    );
  }

  if (!activeOrg) {
    return (
      <div className="rounded border border-slate-800 bg-slate-900 p-8 text-center">
        <p className="text-data text-slate-100">
          {orgs.length === 0 ? 'You are not a member of any organisation.' : 'No organisation selected.'}
        </p>
        <p className="mt-1 text-data text-slate-400">
          {orgs.length === 0
            ? 'Ask an administrator to invite you, or create one from the organisation switcher.'
            : 'Pick one from the switcher in the top bar to continue.'}
        </p>
      </div>
    );
  }

  return <>{children}</>;
}
