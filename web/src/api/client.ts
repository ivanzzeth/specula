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
  UsersResponse,
  EventsResponse,
  ConfigResponse,
  CreateUserRequest,
  PatchUserRequest,
} from './types';

const API_PREFIX = '/api/v1';

function url(p: string): string {
  return API_PREFIX + p;
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
  const res = await fetch(url(path), { ...init, headers, credentials: 'include' });
  if (!res.ok) return fail(res);
  if (res.status === 204) return undefined as T;
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

async function reqVoid(path: string, init?: RequestInit): Promise<void> {
  const res = await fetch(url(path), {
    ...init,
    headers: new Headers(init?.headers),
    credentials: 'include',
  });
  if (!res.ok) return fail(res);
}

// ---- Auth ----

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

// ---- Admin: Upstreams ----

export function getUpstreams(): Promise<UpstreamsResponse> {
  return reqJSON<UpstreamsResponse>('/admin/upstreams');
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
