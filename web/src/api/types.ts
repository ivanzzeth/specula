// DTO types matching internal/admin/dto.go field names exactly.

export interface UserDTO {
  id: number;
  email: string;
  name: string;
  system_role: string;
  created_at: string; // RFC3339
}

export interface LoginResponse {
  user: UserDTO;
}

export interface MeResponse {
  user: UserDTO;
  is_admin: boolean;
  /** The caller's active org, resolved server-side (or via the X-Org-Id header). */
  active_org_id?: string;
  active_org_role?: string;
  orgs?: OrgDTO[];
}

export interface ProtocolStat {
  protocol: string;
  bytes: number;
  objects: number;
  oldest_unix: number;
  newest_unix: number;
}

export interface StatsResponse {
  per_protocol: ProtocolStat[];
  total_bytes: number;
  total_objects: number;
  backend_disk_free: number;
  backend_disk_used: number;
}

export interface SeriesPoint {
  unix: number;
  bytes: number;
}

export interface SeriesResponse {
  protocol: string;
  points: SeriesPoint[];
}

// ---- Upstream mirror chains (GET /api/v1/admin/upstreams) --------------------

/** The four upstream health states. `unknown` means NO DATA, not "healthy". */
export type Health = 'up' | 'blocked' | 'probing' | 'unknown';

/**
 * One upstream mirror in a protocol's ordered fallback chain.
 *
 * HONESTY RULES — render "—", never a fabricated value:
 *  · `last_latency_ms` is meaningful only when `has_latency` is true.
 *  · `last_served_unix` is 0 when this mirror has never served a fetch.
 *  · `hit_share` is 0 when the protocol has served nothing at all; use
 *    `served_count === 0` / `health === 'unknown'` to tell "0%" from "no data".
 *
 * All measurements are in-memory and PER-REPLICA: they reset on restart and
 * describe only the instance that answered. They are not cluster-wide.
 */
export interface UpstreamHealth {
  protocol: string;
  /** Logical mirror name from config — the {id} path segment for mutations. */
  name: string;
  url: string;
  official: boolean;

  /** 0-based position in the effective fallback chain; 0 is tried first. */
  order: number;
  /** Effective priority after any runtime reorder. */
  priority: number;
  /** Priority declared in the YAML baseline. */
  config_priority: number;
  /** True when the live order has drifted from the YAML baseline. */
  overridden: boolean;
  /** False when an operator disabled the mirror; it is skipped but still listed. */
  enabled: boolean;

  health: Health;
  blocked: boolean;
  /** Unix seconds the auto-block window expires; 0 when not blocked. */
  blocked_until_unix: number;
  consecutive_failures: number;
  last_err: string;

  /** Time-to-response-headers of the last success. NOT body download time. */
  last_latency_ms: number;
  has_latency: boolean;

  served_count: number;
  /** This mirror's share of its protocol's served misses, 0..1. */
  hit_share: number;
  last_served_unix: number;
}

/** One protocol's complete ordered mirror chain — render as a section/tab. */
export interface ProtocolUpstreams {
  protocol: string;
  /** The fallback chain in effective order (`order` ascending). */
  mirrors: UpstreamHealth[];
  /** Mirror that most recently served a miss; empty when none has. */
  last_served_by: string;
  /** Protocol's total served misses — the denominator behind `hit_share`. */
  total_served: number;
  /**
   * False when this chain is a config-only echo with no instrumentation behind
   * it. When false, every health/latency/serve field is unmeasured — show "—".
   */
  live: boolean;
}

export interface UpstreamsResponse {
  protocols: ProtocolUpstreams[];
}

/**
 * POST /admin/upstreams/{protocol}/reorder body.
 * `order` must list EVERY configured mirror for the protocol exactly once —
 * a partial list is rejected with 400 rather than half-applied.
 */
export interface ReorderUpstreamsRequest {
  order: string[];
}

/** PATCH /admin/upstreams/{protocol}/{id} body. Omit a field to leave it alone. */
export interface PatchUpstreamRequest {
  enabled?: boolean;
}

export interface UsersResponse {
  users: UserDTO[];
  total: number;
}

export interface VerificationEvent {
  id: number;
  unix: number;
  protocol: string;
  artifact: string;
  digest: string;
  tier: string;
  result: 'pass' | 'fail' | 'warn';
  detail: string;
}

export interface EventsResponse {
  events: VerificationEvent[];
}

export interface UpstreamConfig {
  name: string;
  base_url: string;
  priority: number;
  official: boolean;
}

export interface ProtocolConfig {
  protocol: string;
  upstreams: UpstreamConfig[];
  verify_tiers: string[];
  mutable_ttl_seconds: number;
}

export interface ConfigResponse {
  data_plane_addr: string;
  control_plane_addr: string;
  blob_driver: string;
  meta_driver: string;
  protocols: ProtocolConfig[];
}

export interface CreateUserRequest {
  email: string;
  name: string;
  password: string;
  system_role: string;
}

export interface PatchUserRequest {
  name?: string;
  system_role?: string;
  password?: string;
}


// ---- Cache browser (GET /api/v1/admin/cache/{protocol}) ----------------------

/** The eight browsable protocols. `go` and `gomod` are both accepted. */
export type Protocol = 'oci' | 'pypi' | 'npm' | 'go' | 'gomod' | 'apt' | 'helm' | 'git' | 'tarball';

/** The four verification tiers (PRD §G2), weakest → strongest. */
export type Tier = 'checksum' | 'tofu' | 'consensus' | 'signed';

/** Sort columns accepted by the cache browser. */
export type CacheSort = 'created_at' | 'size' | 'name' | 'verified_at';

/**
 * One cached artifact.
 *
 * HONESTY: there is NO hit/pull-count and NO "last pulled" field, because the
 * serve path does not increment a per-entry counter. Do not invent one.
 * `first_cached_unix` is the entry's created_at (first fetch).
 */
export interface CacheEntryDTO {
  /** Opaque URL-safe id — the {id} segment for delete/pin, and the list key. */
  id: string;
  protocol: string;
  /** Artifact name in its protocol's idiom (OCI repo, package, module path…). */
  name: string;
  /** Version/reference in its protocol's idiom (tag, version, @v file, suite…). */
  version: string;
  /** CAS key, "sha256:…". */
  digest: string;
  size: number;
  /** Tier actually ACHIEVED (not the configured target). Colour by this. */
  tier: Tier | string;
  upstream: string;
  etag?: string;
  /**
   * True when this ref is routed through the short-TTL mutable tier.
   * ALWAYS false on the PostgreSQL backend — do not build UI depending on it.
   */
  mutable: boolean;
  /** True when an operator has protected this entry from GC. */
  pinned: boolean;
  verified_unix: number;
  first_cached_unix: number;
}

export interface CacheEntriesResponse {
  entries: CacheEntryDTO[];
  /** Rows matching the filter, ignoring the page window — size the pager off this. */
  total: number;
  /** The window actually applied, after server-side clamping. */
  limit: number;
  offset: number;
}

/** Query parameters for the cache browser. An invalid value is a 400. */
export interface CacheQuery {
  /** Name contains (literal — % and _ are NOT wildcards). */
  name?: string;
  tier?: Tier;
  upstream?: string;
  pinned?: boolean;
  /** Default: created_at. */
  sort?: CacheSort;
  /** Default: desc (newest first). */
  order?: 'asc' | 'desc';
  /** Server clamps to 500 max; default 50. */
  limit?: number;
  offset?: number;
}

/** POST /admin/cache/{protocol}/{id}/pin body. */
export interface PinCacheEntryRequest {
  pinned: boolean;
}

// ---- Orgs / members / keys ---------------------------------------------------

export interface OrgDTO {
  id: string;
  name: string;
  slug: string;
  status: string;
  created_by: string;
  created_at: string;
  /** The caller's effective role in this org, when known. */
  role?: string;
}

export interface OrgsResponse {
  orgs: OrgDTO[];
}

export interface CreateOrgRequest {
  name: string;
  slug: string;
}

export interface MemberDTO {
  id?: string;
  org_id: string;
  email: string;
  /** viewer < editor < admin < owner */
  role: string;
  invited_by?: string;
  created_at: string;
}

export interface MembersResponse {
  members: MemberDTO[];
}

export interface AddMemberRequest {
  email: string;
  role: string;
}

export interface PatchMemberRequest {
  role?: string;
}

export interface InvitationDTO {
  id: string;
  org_id: string;
  email: string;
  role: string;
  invited_by?: string;
  token?: string;
  status: string;
  expires_at?: string;
  created_at: string;
}

export interface CreateInvitationRequest {
  email: string;
  role: string;
  expires_at?: string;
}

export interface KeyDTO {
  id: string;
  org_id: string;
  label?: string;
  prefix: string;
  created_at: string;
  last_used_at?: string;
  expires_at?: string;
  revoked: boolean;
  /** Plaintext key — returned EXACTLY ONCE at creation. Never retrievable again. */
  raw_key?: string;
}

export interface KeysResponse {
  keys: KeyDTO[];
}

export interface CreateKeyRequest {
  label?: string;
}

// ---- Hosted repos (/api/v1/orgs/{org}/repos) ---------------------------------

export type Visibility = 'private' | 'public';

/**
 * A hosted repository.
 *
 * HONESTY: `size_bytes` is the sum of the tagged MANIFEST sizes — it does NOT
 * include layer blobs, because the repo model stores tag→manifest pointers and
 * never walks manifests. Label it "manifest size" or omit it; never present it
 * as the image pull size.
 */
export interface RepoDTO {
  id: string;
  org_id: string;
  /** Full repo name, canonically "<org>/<repo>" — the pull reference after the host. */
  name: string;
  visibility: Visibility | string;
  /** acl subject string of the creator ("user:…" / "apikey:…"). */
  owner_user_id?: string;
  created_at: string;
  /** Number of tags. 0 is a real count, not "unknown". */
  tag_count: number;
  /** Sum of tagged manifest sizes — see the note above. */
  size_bytes: number;
  /** Most recent tag push; zero-valued when the repo has no tags. */
  last_pushed_at?: string;
}

export interface ReposResponse {
  repos: RepoDTO[];
}

/** PATCH /orgs/{org}/repos/{repo} body. An unknown value is rejected with 400. */
export interface PatchRepoRequest {
  visibility?: Visibility;
}

/**
 * One tag→digest pointer.
 *
 * HONESTY: `arch` is ALWAYS EMPTY today — architecture lives in the image
 * config blob, which nothing parses. Render "—"; never fabricate it.
 * `size` is the manifest's own size, not the image's total pull size.
 */
export interface TagDTO {
  tag: string;
  digest: string;
  size: number;
  arch?: string;
  pushed_at: string;
}

export interface TagsResponse {
  tags: TagDTO[];
}
