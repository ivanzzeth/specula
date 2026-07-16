// API client for Specula /api/v1.
// Auth: httpOnly `specula_session` cookie (set by login/register).
// All requests use credentials:'include' so the cookie travels automatically.
// Non-2xx responses throw ApiError with status code and server detail.

import type {
  UserDTO,
  LoginResponse,
  MeResponse,
  StatsResponse,
  SeriesResponse,
  UpstreamsResponse,
  ProtocolUpstreams,
  ReorderUpstreamsRequest,
  PatchUpstreamRequest,
  UsersResponse,
  EventsResponse,
  ConfigResponse,
  CreateUserRequest,
  PatchUserRequest,
  CacheEntriesResponse,
  CacheQuery,
  OrgDTO,
  OrgsResponse,
  CreateOrgRequest,
  MembersResponse,
  InvitationsResponse,
  MemberDTO,
  AddMemberRequest,
  PatchMemberRequest,
  InvitationDTO,
  CreateInvitationRequest,
  KeyDTO,
  KeysResponse,
  CreateKeyRequest,
  RepoDTO,
  ReposResponse,
  PatchRepoRequest,
  TagsResponse,
} from './types';

const API_PREFIX = '/api/v1';

function url(p: string): string {
  return API_PREFIX + p;
}

/**
 * ── Active org (X-Org-Id) ────────────────────────────────────────────────────
 *
 * The server resolves the caller's active org from the X-Org-Id header. The
 * org switcher sets it here once, and every subsequent request carries it —
 * so no call site has to thread an org id through by hand.
 *
 * Org-SCOPED routes (/orgs/{org}/…) name their org in the PATH and do not
 * depend on this header; it is what disambiguates the org-CONTEXT routes
 * (/me, /keys) for a user who belongs to several orgs.
 */
let activeOrgId: string | null = null;

/** setActiveOrg sets (or clears, with null) the X-Org-Id sent on every request. */
export function setActiveOrg(orgId: string | null): void {
  activeOrgId = orgId;
}

/** getActiveOrg returns the org id currently being sent as X-Org-Id. */
export function getActiveOrg(): string | null {
  return activeOrgId;
}

/** qs builds a query string from defined params only, URL-encoding each value. */
function qs(params: Record<string, string | number | boolean | undefined>): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== '') p.set(k, String(v));
  }
  const out = p.toString();
  return out ? `?${out}` : '';
}

/**
 * seg URL-encodes a single path segment.
 *
 * Required for cache entry ids and mirror names, and for repo/tag names that
 * may contain characters that would otherwise change the path's shape.
 */
function seg(v: string): string {
  return encodeURIComponent(v);
}

// Common HTTP status hints for user-facing error messages.
const STATUS_HINTS: Record<number, string> = {
  400: 'Bad request',
  401: 'Not authenticated — please log in again',
  403: 'Forbidden — admin access required',
  404: 'Not found',
  409: 'Conflict',
  500: 'Internal server error — please retry',
  501: 'Not implemented',
  503: 'Service unavailable',
};

export class ApiError extends Error {
  readonly status: number;
  readonly detail: string;

  constructor(status: number, statusText: string, detail: string) {
    const hint = STATUS_HINTS[status] ?? 'Request failed';
    super(`${hint} (HTTP ${status}${detail ? `: ${detail}` : ''})`);
    this.name = 'ApiError';
    this.status = status;
    this.detail = detail || statusText;
  }
}

async function fail(res: Response): Promise<never> {
  let body = '';
  try {
    body = await res.text();
  } catch {
    /* ignore */
  }
  let detail = body;
  try {
    const j = JSON.parse(body) as { error?: unknown };
    if (j && typeof j.error === 'string') detail = j.error;
  } catch {
    /* not JSON */
  }
  throw new ApiError(res.status, res.statusText, detail.trim());
}

async function reqJSON<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers);
  if (init?.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (activeOrgId && !headers.has('X-Org-Id')) headers.set('X-Org-Id', activeOrgId);
  const res = await fetch(url(path), { ...init, headers, credentials: 'include' });
  if (!res.ok) return fail(res);
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

async function reqVoid(path: string, init?: RequestInit): Promise<void> {
  const headers = new Headers(init?.headers);
  if (init?.body !== undefined && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json');
  }
  if (activeOrgId && !headers.has('X-Org-Id')) headers.set('X-Org-Id', activeOrgId);
  const res = await fetch(url(path), { ...init, headers, credentials: 'include' });
  if (!res.ok) return fail(res);
}

// ---- Auth ----

export interface InstanceResponse {
  /** host:port for `docker login`/`docker push` — the DATA plane, not window.location.host. */
  registry_host: string;
}

/**
 * Deployment facts the browser cannot derive. The UI is served by the control
 * plane while the registry answers on the data plane — a different port locally
 * and usually a different hostname behind an Ingress — so window.location.host
 * is the wrong host to print into a docker command.
 */
export function getInstance(): Promise<InstanceResponse> {
  return reqJSON<InstanceResponse>('/instance');
}

export function getMe(): Promise<MeResponse> {
  return reqJSON<MeResponse>('/me');
}

export function register(email: string, password: string): Promise<LoginResponse> {
  return reqJSON<LoginResponse>('/auth/register', {
    method: 'POST',
    body: JSON.stringify({ email, password }),
  });
}

export function login(email: string, password: string): Promise<LoginResponse> {
  return reqJSON<LoginResponse>('/auth/login', {
    method: 'POST',
    body: JSON.stringify({ email, password }),
  });
}

export function logout(): Promise<void> {
  return reqVoid('/auth/logout', { method: 'POST' });
}

// ---- Admin: Stats ----

export function getStats(): Promise<StatsResponse> {
  return reqJSON<StatsResponse>('/admin/stats');
}

export function getStatsSeries(protocol?: string): Promise<SeriesResponse> {
  const qs = protocol ? `?protocol=${encodeURIComponent(protocol)}` : '';
  return reqJSON<SeriesResponse>(`/admin/stats/series${qs}`);
}

// ---- Admin: Upstream mirror chains (REGISTRY-DESIGN §5.3) ----
// Every mutation returns the protocol's UPDATED chain, so a caller can render
// the new state directly instead of re-fetching (and briefly showing stale
// order/health between the two round-trips).

/** GET /admin/upstreams — every protocol's ordered mirror chain with live state. */
export function getUpstreams(): Promise<UpstreamsResponse> {
  return reqJSON<UpstreamsResponse>('/admin/upstreams');
}

/**
 * POST /admin/upstreams/{protocol}/reorder — set the fallback order.
 * `order` must list every configured mirror exactly once (else 400).
 */
export function reorderUpstreams(
  protocol: string,
  body: ReorderUpstreamsRequest
): Promise<ProtocolUpstreams> {
  return reqJSON<ProtocolUpstreams>(`/admin/upstreams/${seg(protocol)}/reorder`, {
    method: 'POST',
    body: JSON.stringify(body),
  });
}

/** PATCH /admin/upstreams/{protocol}/{name} — enable/disable one mirror. */
export function patchUpstream(
  protocol: string,
  name: string,
  body: PatchUpstreamRequest
): Promise<ProtocolUpstreams> {
  return reqJSON<ProtocolUpstreams>(`/admin/upstreams/${seg(protocol)}/${seg(name)}`, {
    method: 'PATCH',
    body: JSON.stringify(body),
  });
}

/**
 * POST /admin/upstreams/{protocol}/{name}/unblock — clear the auto-block.
 * Idempotent: unblocking a healthy mirror is a no-op.
 */
export function unblockUpstream(protocol: string, name: string): Promise<ProtocolUpstreams> {
  return reqJSON<ProtocolUpstreams>(`/admin/upstreams/${seg(protocol)}/${seg(name)}/unblock`, {
    method: 'POST',
  });
}

// ---- Admin: Cache browser (REGISTRY-DESIGN §5.2) ----

/**
 * GET /admin/cache/{protocol} — paginated listing of what is actually cached.
 * An invalid filter value is a 400 rather than being silently ignored.
 */
export function listCacheEntries(
  protocol: string,
  query: CacheQuery = {}
): Promise<CacheEntriesResponse> {
  return reqJSON<CacheEntriesResponse>(`/admin/cache/${seg(protocol)}${qs({ ...query })}`);
}

/**
 * DELETE /admin/cache/{protocol}/{id} — evict one cache entry.
 * Removes the metadata row; the shared CAS blob is left for GC (it may back
 * other artifacts with the same digest).
 */
export function deleteCacheEntry(protocol: string, id: string): Promise<void> {
  return reqVoid(`/admin/cache/${seg(protocol)}/${seg(id)}`, { method: 'DELETE' });
}

/** POST /admin/cache/{protocol}/{id}/pin — protect an entry from GC (or unprotect). */
export function pinCacheEntry(protocol: string, id: string, pinned: boolean): Promise<void> {
  return reqVoid(`/admin/cache/${seg(protocol)}/${seg(id)}/pin`, {
    method: 'POST',
    body: JSON.stringify({ pinned }),
  });
}

// ---- Orgs ----

export function listOrgs(): Promise<OrgsResponse> {
  return reqJSON<OrgsResponse>('/orgs');
}

export function createOrg(body: CreateOrgRequest): Promise<OrgDTO> {
  return reqJSON<OrgDTO>('/orgs', { method: 'POST', body: JSON.stringify(body) });
}

export function getOrg(id: string): Promise<OrgDTO> {
  return reqJSON<OrgDTO>(`/orgs/${seg(id)}`);
}

// ---- Members (org admin) ----

export function listMembers(orgId: string): Promise<MembersResponse> {
  return reqJSON<MembersResponse>(`/orgs/${seg(orgId)}/members`);
}

export function addMember(orgId: string, body: AddMemberRequest): Promise<MemberDTO> {
  return reqJSON<MemberDTO>(`/orgs/${seg(orgId)}/members`, {
    method: 'POST',
    body: JSON.stringify(body),
  });
}

export function patchMember(
  orgId: string,
  email: string,
  body: PatchMemberRequest
): Promise<MemberDTO> {
  return reqJSON<MemberDTO>(`/orgs/${seg(orgId)}/members/${seg(email)}`, {
    method: 'PATCH',
    body: JSON.stringify(body),
  });
}

export function removeMember(orgId: string, email: string): Promise<void> {
  return reqVoid(`/orgs/${seg(orgId)}/members/${seg(email)}`, { method: 'DELETE' });
}

// ---- Invitations ----

export function createInvitation(
  orgId: string,
  body: CreateInvitationRequest
): Promise<InvitationDTO> {
  return reqJSON<InvitationDTO>(`/orgs/${seg(orgId)}/invitations`, {
    method: 'POST',
    body: JSON.stringify(body),
  });
}

export function listInvitations(orgId: string): Promise<InvitationsResponse> {
  return reqJSON<InvitationsResponse>(`/orgs/${seg(orgId)}/invitations`);
}

/**
 * Respond to an invitation. The invitation's status is a property of the
 * resource, so accepting is a PATCH of it rather than an action endpoint.
 *
 * Accepting returns the membership row that was just written — the caller uses
 * its org_id to switch straight into the org they joined.
 *
 * Error taxonomy the caller should surface distinctly:
 *   403 — the invitation is addressed to a different email
 *   404 — no such invitation
 *   409 — already accepted/declined (a spent token)
 *   410 — expired
 */
export function acceptInvitation(token: string): Promise<MemberDTO> {
  return reqJSON<MemberDTO>(`/invitations/${seg(token)}`, {
    method: 'PATCH',
    body: JSON.stringify({ status: 'accepted' }),
  });
}

export function declineInvitation(token: string): Promise<InvitationDTO> {
  return reqJSON<InvitationDTO>(`/invitations/${seg(token)}`, {
    method: 'PATCH',
    body: JSON.stringify({ status: 'declined' }),
  });
}

// ---- API keys ----
// The org a key belongs to comes from the active org context (X-Org-Id).

/**
 * POST /keys — create an API key.
 * The response's `raw_key` is the ONLY time the plaintext is ever available:
 * show it once, offer a copy, and never expect to retrieve it again.
 */
export function createKey(body: CreateKeyRequest): Promise<KeyDTO> {
  return reqJSON<KeyDTO>('/keys', { method: 'POST', body: JSON.stringify(body) });
}

export function listKeys(): Promise<KeysResponse> {
  return reqJSON<KeysResponse>('/keys');
}

export function revokeKey(id: string): Promise<void> {
  return reqVoid(`/keys/${seg(id)}`, { method: 'DELETE' });
}

// ---- Hosted repos (REGISTRY-DESIGN §5.1) ----
// `org` accepts an org slug or id. `repo` is the BARE repo name (a single path
// segment) — e.g. name "acme/app" is addressed as org "acme", repo "app".

export function listRepos(org: string): Promise<ReposResponse> {
  return reqJSON<ReposResponse>(`/orgs/${seg(org)}/repos`);
}

export function getRepo(org: string, repo: string): Promise<RepoDTO> {
  return reqJSON<RepoDTO>(`/orgs/${seg(org)}/repos/${seg(repo)}`);
}

/** PATCH /orgs/{org}/repos/{repo} — flip visibility (org admin/owner). */
export function patchRepo(org: string, repo: string, body: PatchRepoRequest): Promise<RepoDTO> {
  return reqJSON<RepoDTO>(`/orgs/${seg(org)}/repos/${seg(repo)}`, {
    method: 'PATCH',
    body: JSON.stringify(body),
  });
}

export function deleteRepo(org: string, repo: string): Promise<void> {
  return reqVoid(`/orgs/${seg(org)}/repos/${seg(repo)}`, { method: 'DELETE' });
}

export function listRepoTags(org: string, repo: string): Promise<TagsResponse> {
  return reqJSON<TagsResponse>(`/orgs/${seg(org)}/repos/${seg(repo)}/tags`);
}

/**
 * DELETE /orgs/{org}/repos/{repo}/tags/{tag} — remove a tag pointer.
 * The manifest and layers stay in the shared CAS (other tags may reference
 * them); reclaiming unreferenced blobs is GC's job.
 */
export function deleteRepoTag(org: string, repo: string, tag: string): Promise<void> {
  return reqVoid(`/orgs/${seg(org)}/repos/${seg(repo)}/tags/${seg(tag)}`, { method: 'DELETE' });
}

// ---- Admin: Users ----

export function listUsers(limit?: number, offset?: number): Promise<UsersResponse> {
  const p = new URLSearchParams();
  if (limit != null) p.set('limit', String(limit));
  if (offset != null) p.set('offset', String(offset));
  const qs = p.toString();
  return reqJSON<UsersResponse>(`/admin/users${qs ? `?${qs}` : ''}`);
}

export function createUser(body: CreateUserRequest): Promise<UserDTO> {
  return reqJSON<UserDTO>('/admin/users', {
    method: 'POST',
    body: JSON.stringify(body),
  });
}

export function getUser(id: number): Promise<UserDTO> {
  return reqJSON<UserDTO>(`/admin/users/${id}`);
}

export function patchUser(id: number, body: PatchUserRequest): Promise<UserDTO> {
  return reqJSON<UserDTO>(`/admin/users/${id}`, {
    method: 'PATCH',
    body: JSON.stringify(body),
  });
}

export function deleteUser(id: number): Promise<void> {
  return reqVoid(`/admin/users/${id}`, { method: 'DELETE' });
}

// ---- Admin: Config ----

export function getConfig(): Promise<ConfigResponse> {
  return reqJSON<ConfigResponse>('/admin/config');
}

// ---- Admin: Events ----

export function getEvents(limit?: number): Promise<EventsResponse> {
  const qs = limit != null ? `?limit=${limit}` : '';
  return reqJSON<EventsResponse>(`/admin/events${qs}`);
}
