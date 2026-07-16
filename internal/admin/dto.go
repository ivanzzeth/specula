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

// ---- upstream health (GET /api/v1/admin/upstreams) ---------------------------

// UpstreamHealth reports the auto-block state of one upstream mirror. Blocked is
// true while the circuit breaker has tripped; LastErr carries the most recent
// failure reason (empty when healthy).
type UpstreamHealth struct {
	Protocol string `json:"protocol"`
	URL      string `json:"url"`
	Blocked  bool   `json:"blocked"`
	LastErr  string `json:"last_err"`
}

// UpstreamsResponse is the upstream-health list payload.
type UpstreamsResponse struct {
	Upstreams []UpstreamHealth `json:"upstreams"`
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

// MeResponse is the GET /api/v1/me payload (the currently authenticated user).
type MeResponse struct {
	User UserDTO `json:"user"`
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

// ---- error envelope ----------------------------------------------------------

// ErrorResponse is the uniform error envelope for every non-2xx JSON response.
type ErrorResponse struct {
	Error string `json:"error"`
}
