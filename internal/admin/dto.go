// Package admin defines the Specula control-plane (management) HTTP API: typed
// request/response DTOs, a dependency-injected Server, and route registration
// under /api/v1. The data plane (artifact proxy) is served separately; this
// package is the authenticated management surface consumed by the embedded
// WebUI and by CLI/automation via Bearer tokens.
//
// This file (dto.go) is the wire contract. Field names and JSON tags here are
// the source of truth the WebUI codes against; keep them stable.
package admin

import "time"

// ---- capacity statistics (GET /api/v1/admin/stats) ---------------------------

// ProtocolStat is the per-protocol capacity aggregate (G7). OldestUnix and
// NewestUnix are Unix seconds (0 when the protocol has no cached objects).
type ProtocolStat struct {
	Protocol   string `json:"protocol"`
	Bytes      int64  `json:"bytes"`
	Objects    int64  `json:"objects"`
	OldestUnix int64  `json:"oldest_unix"`
	NewestUnix int64  `json:"newest_unix"`
}

// StatsResponse is the cache-capacity dashboard payload: per-protocol rows plus
// grand totals and the backend disk footprint (statfs / S3 UsageBytes).
type StatsResponse struct {
	PerProtocol     []ProtocolStat `json:"per_protocol"`
	TotalBytes      int64          `json:"total_bytes"`
	TotalObjects    int64          `json:"total_objects"`
	BackendDiskFree int64          `json:"backend_disk_free"`
	BackendDiskUsed int64          `json:"backend_disk_used"`
}

// ---- time-series (GET /api/v1/admin/stats/series) ----------------------------

// SeriesPoint is one sample in a cache-size time series (ring buffer).
type SeriesPoint struct {
	Unix  int64 `json:"unix"`
	Bytes int64 `json:"bytes"`
}

// SeriesResponse holds the historical cache-size curve. When Protocol is empty
// the series is the grand total across all protocols.
type SeriesResponse struct {
	Protocol string        `json:"protocol"`
	Points   []SeriesPoint `json:"points"`
}

// ---- upstream mirrors (GET /api/v1/admin/upstreams) --------------------------

// UpstreamHealth reports the state of one upstream mirror in a protocol's
// ordered fallback chain (REGISTRY-DESIGN §5.3).
//
// # Honesty rules for the UI
//
// Several fields are only meaningful when their companion flag says so. Render
// "—", never a fabricated zero:
//
//   - LastLatencyMs is valid only when HasLatency is true.
//   - LastServedUnix is 0 when this mirror has never served a fetch.
//   - HitShare is 0 when the protocol has served no misses at all; check
//     Health=="unknown" / ServedCount==0 to distinguish "0%" from "no data".
//
// All measurements are in-memory and per-replica: they reset on restart and
// describe only the instance that answered this request. They are NOT
// cluster-wide totals.
type UpstreamHealth struct {
	Protocol string `json:"protocol"`
	// Name is the mirror's logical name from config; it is the {id} path
	// segment for the PATCH / unblock endpoints and the element of the
	// reorder request body.
	Name string `json:"name"`
	URL  string `json:"url"`
	// Official reports whether config marks this mirror as the authoritative
	// origin (as opposed to a third-party mirror).
	Official bool `json:"official"`

	// Order is the mirror's 0-based position in the effective fallback chain:
	// 0 is tried first. Present on every row and always dense.
	Order int `json:"order"`
	// Priority is the effective priority value after any runtime reorder.
	Priority int `json:"priority"`
	// ConfigPriority is the priority declared in the YAML baseline.
	ConfigPriority int `json:"config_priority"`
	// Overridden is true when Priority came from a runtime reorder rather than
	// config — i.e. the live chain has drifted from the declarative baseline.
	Overridden bool `json:"overridden"`
	// Enabled is false when an operator disabled this mirror at runtime. A
	// disabled mirror is skipped by the fallback chain but still listed here.
	Enabled bool `json:"enabled"`

	// Health is one of "up" | "blocked" | "probing" | "unknown".
	// "unknown" means the mirror has not been contacted since process start —
	// it is NOT a synonym for healthy.
	Health string `json:"health"`
	// Blocked is true while the auto-block circuit breaker has tripped.
	// Equivalent to Health == "blocked".
	Blocked bool `json:"blocked"`
	// BlockedUntilUnix is when the auto-block window expires (Unix seconds);
	// 0 when not blocked.
	BlockedUntilUnix int64 `json:"blocked_until_unix"`
	// ConsecutiveFailures is the current transient-failure streak.
	ConsecutiveFailures int `json:"consecutive_failures"`
	// LastErr is the most recent failure reason; empty when healthy.
	LastErr string `json:"last_err"`

	// LastLatencyMs is the most recent successful request's time-to-response-
	// headers, in milliseconds. This is upstream responsiveness, NOT body
	// download time (bodies stream straight through to the client).
	// Meaningful only when HasLatency is true.
	LastLatencyMs int64 `json:"last_latency_ms"`
	// HasLatency reports whether LastLatencyMs has been measured at all.
	HasLatency bool `json:"has_latency"`

	// ServedCount is how many cache-miss fetches this mirror has served since
	// process start.
	ServedCount int64 `json:"served_count"`
	// HitShare is this mirror's share of its protocol's served misses, in
	// [0,1]. It is ServedCount / (sum of ServedCount across the protocol).
	HitShare float64 `json:"hit_share"`
	// LastServedUnix is when this mirror last served a fetch (Unix seconds);
	// 0 when never.
	LastServedUnix int64 `json:"last_served_unix"`
}

// ProtocolUpstreams is one protocol's complete ordered mirror chain — the unit
// the Upstreams ops view renders as a per-protocol section/tab.
type ProtocolUpstreams struct {
	Protocol string `json:"protocol"`
	// Mirrors is the fallback chain in effective order (Order ascending).
	Mirrors []UpstreamHealth `json:"mirrors"`
	// LastServedBy is the name of the mirror that most recently served a miss
	// for this protocol; empty when none has. This is the "who is actually
	// answering right now" signal.
	LastServedBy string `json:"last_served_by"`
	// TotalServed is the protocol's total served misses since process start;
	// the denominator behind each mirror's HitShare.
	TotalServed int64 `json:"total_served"`
	// Live is false when this protocol's chain is a config-only echo with no
	// live instrumentation behind it (no upstream Registry wired in). When
	// false, every health/latency/serve field on its mirrors is unmeasured and
	// must be rendered as "—".
	Live bool `json:"live"`
}

// UpstreamsResponse is the per-protocol mirror-chain payload. Protocols are
// sorted by name for stable rendering.
type UpstreamsResponse struct {
	Protocols []ProtocolUpstreams `json:"protocols"`
}

// ReorderUpstreamsRequest is the POST /api/v1/admin/upstreams/{protocol}/reorder
// body. Order lists mirror names in the desired fallback order (index 0 tried
// first) and must contain every configured mirror for that protocol exactly
// once — a partial list would leave the resulting order ambiguous, so it is
// rejected rather than half-applied.
type ReorderUpstreamsRequest struct {
	Order []string `json:"order"`
}

// PatchUpstreamRequest is the PATCH /api/v1/admin/upstreams/{protocol}/{id}
// body. Enabled is a pointer so nil means "leave unchanged".
type PatchUpstreamRequest struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// ---- users (GET/POST/PATCH/DELETE /api/v1/admin/users) ------------------------

// UserDTO is the safe, client-facing projection of a user account. The password
// hash is never included.
type UserDTO struct {
	ID         int64     `json:"id"`
	Email      string    `json:"email"`
	Name       string    `json:"name"`
	SystemRole string    `json:"system_role"`
	CreatedAt  time.Time `json:"created_at"`
}

// UsersResponse is the paginated user-list payload.
type UsersResponse struct {
	Users []UserDTO `json:"users"`
	Total int64     `json:"total"`
}

// CreateUserRequest is the POST /api/v1/admin/users body (admin creates an
// account directly). SystemRole defaults to "user" when empty.
type CreateUserRequest struct {
	Email      string `json:"email"`
	Name       string `json:"name"`
	Password   string `json:"password"`
	SystemRole string `json:"system_role"`
}

// PatchUserRequest is the PATCH /api/v1/admin/users/{id} body. Every field is a
// pointer so that a nil value means "leave unchanged" and a present value means
// "set to this". Password, when non-nil, triggers a re-hash.
type PatchUserRequest struct {
	Name       *string `json:"name,omitempty"`
	SystemRole *string `json:"system_role,omitempty"`
	Password   *string `json:"password,omitempty"`
}

// ---- auth (POST /api/v1/auth/{register,login,logout}, GET /api/v1/me) ---------

// RegisterRequest is the public self-registration body. The first account ever
// registered is promoted to system_role="admin". Name is optional; it is
// persisted as the user's display name when non-empty.
type RegisterRequest struct {
	Email    string `json:"email"`
	Name     string `json:"name,omitempty"`
	Password string `json:"password"`
}

// LoginRequest is the public login body. On success a session cookie is set and
// LoginResponse is returned.
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// LoginResponse is returned by both register and login; the session JWT itself
// travels in the httpOnly Set-Cookie header, not the body.
type LoginResponse struct {
	User UserDTO `json:"user"`
}

// MeResponse is the GET /api/v1/me payload. The base user info is always
// present; org context fields are populated when an org store is wired in.
type MeResponse struct {
	User          UserDTO  `json:"user"`
	IsAdmin       bool     `json:"is_admin"`
	ActiveOrgID   string   `json:"active_org_id,omitempty"`
	ActiveOrgRole string   `json:"active_org_role,omitempty"`
	Orgs          []OrgDTO `json:"orgs,omitempty"`
}

// ---- verification events (GET /api/v1/admin/events) --------------------------

// VerificationEvent is one audit record from the verify chain / alerting stream:
// a checksum/TOFU/consensus/signature outcome for a fetched artifact.
type VerificationEvent struct {
	ID       int64  `json:"id"`
	Unix     int64  `json:"unix"`     // event time, Unix seconds
	Protocol string `json:"protocol"` // "oci" | "pypi" | ...
	Artifact string `json:"artifact"` // human ref, e.g. "library/nginx:latest"
	Digest   string `json:"digest"`   // sha256:...
	Tier     string `json:"tier"`     // highest verification tier reached
	Result   string `json:"result"`   // "pass" | "fail" | "warn"
	Detail   string `json:"detail"`   // human-readable explanation (empty on pass)
}

// EventsResponse is the verification-event feed payload.
type EventsResponse struct {
	Events []VerificationEvent `json:"events"`
}

// ---- config snapshot (GET /api/v1/admin/config) ------------------------------

// UpstreamView is a redacted upstream entry for the config snapshot.
type UpstreamView struct {
	Name     string `json:"name"`
	BaseURL  string `json:"base_url"`
	Priority int    `json:"priority"`
	Official bool   `json:"official"`
}

// ProtocolConfigView is a redacted per-protocol config entry.
type ProtocolConfigView struct {
	Protocol      string         `json:"protocol"`
	Upstreams     []UpstreamView `json:"upstreams"`
	VerifyTiers   []string       `json:"verify_tiers"`
	MutableTTLSec int64          `json:"mutable_ttl_seconds"`
}

// ConfigResponse is the running configuration snapshot with all secrets
// (JWT secret, admin key, S3 credentials) redacted.
type ConfigResponse struct {
	DataPlaneAddr    string               `json:"data_plane_addr"`
	ControlPlaneAddr string               `json:"control_plane_addr"`
	BlobDriver       string               `json:"blob_driver"`
	MetaDriver       string               `json:"meta_driver"`
	Protocols        []ProtocolConfigView `json:"protocols"`
}

// ---- orgs (GET/POST /api/v1/orgs, GET /api/v1/orgs/{id}) --------------------

// OrgDTO is the safe client-facing projection of an org.
type OrgDTO struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Slug      string    `json:"slug"`
	Status    string    `json:"status"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	// Role is the caller's effective role in the org; populated by handlers
	// that have access to the member record.
	Role string `json:"role,omitempty"`
}

// CreateOrgRequest is the POST /api/v1/orgs body.
type CreateOrgRequest struct {
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// OrgsResponse is the paginated org-list payload.
type OrgsResponse struct {
	Orgs []OrgDTO `json:"orgs"`
}

// ---- members (GET/POST/PATCH/DELETE /api/v1/orgs/{id}/members) ---------------

// MemberDTO is the client-facing projection of an org_members row.
type MemberDTO struct {
	ID        string    `json:"id,omitempty"`
	OrgID     string    `json:"org_id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InvitedBy string    `json:"invited_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// MembersResponse is the member-list payload.
type MembersResponse struct {
	Members []MemberDTO `json:"members"`
}

// AddMemberRequest is the POST /api/v1/orgs/{id}/members body.
type AddMemberRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// PatchMemberRequest is the PATCH /api/v1/orgs/{id}/members/{email} body.
type PatchMemberRequest struct {
	Role *string `json:"role,omitempty"`
}

// ---- invitations (POST /api/v1/orgs/{id}/invitations) -----------------------

// InvitationDTO is the client-facing projection of an org_invitations row.
type InvitationDTO struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"org_id"`
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	InvitedBy string    `json:"invited_by,omitempty"`
	Token     string    `json:"token,omitempty"` // omitted after creation unless admin
	Status    string    `json:"status"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// InvitationsResponse is the invitation-list payload.
type InvitationsResponse struct {
	Invitations []InvitationDTO `json:"invitations"`
}

// CreateInvitationRequest is the POST /api/v1/orgs/{id}/invitations body.
type CreateInvitationRequest struct {
	Email     string    `json:"email"`
	Role      string    `json:"role"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

// AcceptInvitationRequest is the POST /api/v1/invitations/accept body.
type AcceptInvitationRequest struct {
	Token string `json:"token"`
}

// ---- api keys (POST/GET/DELETE /api/v1/keys) ---------------------------------

// KeyDTO is the client-facing projection of an api_keys row. The raw plaintext
// key is only present immediately after creation (RawKey field); it is never
// stored and never returned again after that single response.
type KeyDTO struct {
	ID         string     `json:"id"`
	OrgID      string     `json:"org_id"`
	Label      string     `json:"label,omitempty"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	Revoked    bool       `json:"revoked"`
	// RawKey is the plaintext key returned exactly once at creation time.
	RawKey string `json:"raw_key,omitempty"`
}

// KeysResponse is the key-list payload.
type KeysResponse struct {
	Keys []KeyDTO `json:"keys"`
}

// CreateKeyRequest is the POST /api/v1/keys body.
type CreateKeyRequest struct {
	Label string `json:"label,omitempty"`
}

// ---- cache browser (GET /api/v1/admin/cache/{protocol}) ----------------------

// CacheEntryDTO is one cached artifact as shown in the cache browser
// (REGISTRY-DESIGN §5.2): what is cached, how big, where it came from, and how
// well it was verified.
//
// # Honesty rules for the UI
//
// There is deliberately NO hit/pull count field: the serve path does not
// currently increment a per-entry counter, so any such number would be invented.
// See the R3 handover notes.
//
// FirstCachedUnix is the entry's created_at (first fetch). There is no
// "last pulled" timestamp for the same reason.
type CacheEntryDTO struct {
	// ID is the opaque, URL-safe entry identifier — the {id} segment for the
	// delete and pin endpoints, and a stable key for list rendering. It encodes
	// (protocol, name, version); it is not a secret and carries no authority.
	ID string `json:"id"`

	Protocol string `json:"protocol"`
	// Name is the artifact name in its protocol's own idiom: an OCI repository
	// ("library/nginx"), a PyPI/npm package, a Go module path, an apt pool
	// path, a Helm chart, a git repo, a tarball URL key.
	Name string `json:"name"`
	// Version is the protocol's version/reference idiom: an OCI tag, a package
	// version, a Go @v file, an apt suite, a git ref.
	Version string `json:"version"`
	// Digest is the CAS key ("sha256:…") — the content identity.
	Digest string `json:"digest"`
	// Size is the artifact's byte length.
	Size int64 `json:"size"`

	// Tier is the verification tier actually reached, one of
	// "signed" | "consensus" | "tofu" | "checksum" (PRD §G2). This is the
	// honest achieved tier, not the configured target — render it semantically.
	Tier string `json:"tier"`
	// Upstream is the mirror the bytes were fetched from.
	Upstream string `json:"upstream"`
	// ETag is the upstream ETag recorded at fetch time; often empty.
	ETag string `json:"etag,omitempty"`

	// Mutable is true when this ref is routed through the short-TTL mutable
	// tier (a tag/index/ref) rather than being immutable content.
	// Always false on the PostgreSQL backend, whose cache_entries table has no
	// mutable column — do not build UI that depends on it.
	Mutable bool `json:"mutable"`
	// Pinned is true when an operator has protected this entry from GC.
	Pinned bool `json:"pinned"`

	// VerifiedUnix is when verification passed (Unix seconds).
	VerifiedUnix int64 `json:"verified_unix"`
	// FirstCachedUnix is when the entry was first written (Unix seconds).
	FirstCachedUnix int64 `json:"first_cached_unix"`
}

// CacheEntriesResponse is the paginated cache-browser payload. Total counts every
// entry matching the filter, ignoring the page window, so the pager can size
// itself; Limit/Offset echo the window actually applied after clamping.
type CacheEntriesResponse struct {
	Entries []CacheEntryDTO `json:"entries"`
	Total   int64           `json:"total"`
	Limit   int             `json:"limit"`
	Offset  int             `json:"offset"`
}

// PinCacheEntryRequest is the POST /api/v1/admin/cache/{protocol}/{id}/pin body.
type PinCacheEntryRequest struct {
	// Pinned sets (true) or clears (false) the eviction-protection flag.
	Pinned bool `json:"pinned"`
}

// ---- hosted repos (/api/v1/orgs/{org}/repos) ---------------------------------

// RepoDTO is the client-facing projection of a hosted repository.
type RepoDTO struct {
	ID    string `json:"id"`
	OrgID string `json:"org_id"`
	// Name is the full repository name, canonically "<org>/<repo>" — exactly
	// what a `docker pull` reference uses after the host.
	Name string `json:"name"`
	// Visibility is "private" | "public".
	Visibility string `json:"visibility"`
	// OwnerUserID is the acl subject string of the creator ("user:…"/"apikey:…").
	OwnerUserID string    `json:"owner_user_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`

	// TagCount is the number of tags in this repo. Populated on the detail
	// endpoint and on list responses; 0 is a real count, not "unknown".
	TagCount int `json:"tag_count"`
	// SizeBytes is the total size of the repo's tagged manifests as recorded in
	// the cache metadata, or 0 when unknown.
	//
	// HONESTY: this is NOT the full pull size of the images. It does not sum the
	// layer blobs, because the repo model records tag→manifest-digest pointers
	// and does not walk manifests to enumerate their layers. Label it as
	// manifest size, or omit it — do not present it as "image size".
	SizeBytes int64 `json:"size_bytes"`
	// LastPushedAt is the most recent tag update time; zero when the repo has
	// no tags.
	LastPushedAt time.Time `json:"last_pushed_at,omitempty"`
}

// ReposResponse is the repo-list payload.
type ReposResponse struct {
	Repos []RepoDTO `json:"repos"`
}

// PatchRepoRequest is the PATCH /api/v1/orgs/{org}/repos/{repo} body. Visibility
// is a pointer so nil means "leave unchanged".
type PatchRepoRequest struct {
	// Visibility is "private" | "public". Any other value is rejected rather
	// than normalized, so a typo cannot silently make a repo private.
	Visibility *string `json:"visibility,omitempty"`
}

// TagDTO is one tag→digest pointer in a hosted repo.
type TagDTO struct {
	Tag string `json:"tag"`
	// Digest is the manifest digest this tag currently resolves to.
	Digest string `json:"digest"`
	// Size is the tagged manifest's byte size, or 0 when the cache metadata has
	// no record of it. This is the manifest's own size, NOT the image's total
	// pull size — see RepoDTO.SizeBytes.
	Size int64 `json:"size"`
	// Arch is the tagged image's architecture.
	//
	// HONESTY: this is ALWAYS EMPTY today. Architecture lives inside the image
	// config blob, which the push path does not parse, so nothing records it.
	// The field is present because the design calls for the column; render "—"
	// until a manifest-inspection pass populates it. Do not fabricate a value.
	Arch string `json:"arch,omitempty"`
	// PushedAt is when this tag was last pushed.
	PushedAt time.Time `json:"pushed_at"`
}

// TagsResponse is the tag-list payload for one hosted repo.
type TagsResponse struct {
	Tags []TagDTO `json:"tags"`
}

// ---- error envelope ----------------------------------------------------------

// ErrorResponse is the uniform error envelope for every non-2xx JSON response.
type ErrorResponse struct {
	Error string `json:"error"`
}

// InstanceResponse is GET /api/v1/instance — deployment facts the browser
// cannot derive on its own.
type InstanceResponse struct {
	// RegistryHost is the host:port for `docker login` / `docker push`, i.e. the
	// data plane. Never window.location.host, which is the control plane.
	RegistryHost string `json:"registry_host"`
}
