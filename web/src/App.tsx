import { createBrowserRouter } from 'react-router-dom';

import { RequireAuth, RequireAdmin } from './components/auth';
import { OrgProvider, RequireOrg } from './components/org-context';
import { Layout } from './pages/Layout';
import { Dashboard } from './pages/Dashboard';
import { Upstreams } from './pages/Upstreams';
import { Events } from './pages/Events';
import { Users } from './pages/Users';
import { Config } from './pages/Config';
import { Repos } from './pages/Repos';
import { RepoDetail } from './pages/RepoDetail';
import { PushGuide } from './pages/PushGuide';
import { CacheBrowser } from './pages/CacheBrowser';
import { Members } from './pages/Members';
import { InvitationAccept } from './pages/InvitationAccept';
import { Tokens } from './pages/Tokens';

/**
 * Route table — R3 WebUI.
 *
 * ── OWNERSHIP (so the four parallel UI agents never collide) ─────────────────
 *
 *  Agent 1 · REGISTRY  → pages/Repos.tsx, pages/RepoDetail.tsx, pages/PushGuide.tsx
 *                        routes: /repos, /repos/:repo, /push
 *                        API: listRepos, getRepo, patchRepo, deleteRepo,
 *                             listRepoTags, deleteRepoTag
 *
 *  Agent 2 · CACHE     → pages/CacheBrowser.tsx (+ any components/cache/*)
 *                        routes: /cache, /cache/:protocol
 *                        API: listCacheEntries, deleteCacheEntry, pinCacheEntry
 *
 *  Agent 3 · OPS       → pages/Upstreams.tsx, pages/Events.tsx, pages/Config.tsx
 *                        routes: /upstreams, /events, /config
 *                        API: getUpstreams, reorderUpstreams, patchUpstream,
 *                             unblockUpstream, getEvents, getConfig
 *                        (Upstreams.tsx is already implemented — it is the
 *                         reference for the design language. Extend, don't rewrite.)
 *
 *  Agent 4 · TENANCY   → pages/Members.tsx, pages/Tokens.tsx, pages/Users.tsx,
 *                        pages/Dashboard.tsx
 *                        routes: /members, /tokens, /users, /
 *                        API: listMembers, addMember, patchMember, removeMember,
 *                             createInvitation, createKey, listKeys, revokeKey,
 *                             listUsers/createUser/patchUser/deleteUser,
 *                             getStats, getStatsSeries
 *
 * SHARED — do not edit without coordinating: api/client.ts, api/types.ts,
 * components/ui/*, lib/utils.ts, pages/Layout.tsx, App.tsx.
 *
 * ── GUARD MODEL (R3 integration) ─────────────────────────────────────────────
 *
 *  RequireAuth  — wraps the whole tree. No session → LoginScreen. Everything
 *                 below it can assume a user exists.
 *  RequireOrg   — routes whose every request carries X-Org-Id (Registry zone,
 *                 Members, Tokens). No resolved org → these pages cannot form a
 *                 valid request, so the guard answers instead of the page.
 *  RequireAdmin — system_role=="admin". The Cache and Ops zones are CROSS-TENANT
 *                 (they read every org's data), so org membership is not the
 *                 relevant right — system admin is. This is why cache/ops routes
 *                 are RequireAdmin and NOT RequireOrg: an admin with no org must
 *                 still be able to see what the proxy cached.
 *
 * The guards compose outside-in and mirror the nav's three zones, so a route's
 * guard and its nav visibility (Layout.tsx `adminOnly`) can never disagree.
 */
export const router = createBrowserRouter([
  {
    element: (
      <RequireAuth>
        <OrgProvider>
          <Layout />
        </OrgProvider>
      </RequireAuth>
    ),
    children: [
      // ── Cache zone ────────────────────────────────────────────────────────
      { path: '/', element: <Dashboard /> },
      {
        path: '/cache',
        element: (
          <RequireAdmin>
            <CacheBrowser />
          </RequireAdmin>
        ),
      },
      {
        // The protocol tab is a route so a cache view is linkable/shareable —
        // "look at what pypi cached" must survive being pasted into a ticket.
        path: '/cache/:protocol',
        element: (
          <RequireAdmin>
            <CacheBrowser />
          </RequireAdmin>
        ),
      },
      {
        path: '/events',
        element: (
          <RequireAdmin>
            <Events />
          </RequireAdmin>
        ),
      },

      // ── Invitation acceptance ─────────────────────────────────────────────
      // Deliberately NOT RequireOrg: the invitee has no org — that is why they
      // were sent this link. Guarding it with RequireOrg would refuse the one
      // page that gives them one.
      { path: '/invitations/:token', element: <InvitationAccept /> },

      // ── Registry zone ─────────────────────────────────────────────────────
      // Org-scoped: every request here is X-Org-Id'd, and the push commands are
      // rendered with the org slug baked in.
      {
        path: '/repos',
        element: (
          <RequireOrg>
            <Repos />
          </RequireOrg>
        ),
      },
      {
        path: '/repos/:repo',
        element: (
          <RequireOrg>
            <RepoDetail />
          </RequireOrg>
        ),
      },
      {
        path: '/push',
        element: (
          <RequireOrg>
            <PushGuide />
          </RequireOrg>
        ),
      },

      // ── Ops zone ──────────────────────────────────────────────────────────
      {
        path: '/upstreams',
        element: (
          <RequireAdmin>
            <Upstreams />
          </RequireAdmin>
        ),
      },
      // Members/Tokens are org-scoped: a member is an identity's role *in an
      // org*, and a token is minted *for an org*.
      {
        path: '/members',
        element: (
          <RequireOrg>
            <Members />
          </RequireOrg>
        ),
      },
      {
        path: '/tokens',
        element: (
          <RequireOrg>
            <Tokens />
          </RequireOrg>
        ),
      },
      {
        path: '/users',
        element: (
          <RequireAdmin>
            <Users />
          </RequireAdmin>
        ),
      },
      {
        path: '/config',
        element: (
          <RequireAdmin>
            <Config />
          </RequireAdmin>
        ),
      },
    ],
  },
]);
