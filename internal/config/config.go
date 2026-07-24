// Package config defines the Specula configuration model (server ports, storage
// blob/meta drivers, cache TTLs with -1/0 sentinels, per-protocol upstreams +
// verification) and loads it from a YAML file with optional SPECULA_* environment
// variable overrides.
//
// Environment override convention:
//
//	SPECULA_ prefix, double-underscore (__) as level separator.
//	Example: SPECULA_SERVER__DATA_PLANE_ADDR overrides server.data_plane_addr.
package config

import (
	"fmt"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/v2"
)

// TTL sentinel values (from DESIGN-REVIEW §3, mirrors Nexus convention):
//
//	-1 = never revalidate (treat as immutable CAS content)
//	 0 = revalidate on every request
const (
	TTLNeverRevalidate  int64 = -1
	TTLAlwaysRevalidate int64 = 0
)

// EnvPrefix is the prefix for all environment variable overrides.
const EnvPrefix = "SPECULA_"

// Config is the root configuration model.
type Config struct {
	Server    ServerConfig              `koanf:"server"`
	Storage   StorageConfig             `koanf:"storage"`
	Cache     CacheConfig               `koanf:"cache"`
	Coalesce  CoalesceConfig            `koanf:"coalesce"`
	Auth      AuthConfig                `koanf:"auth"`
	Protocols map[string]ProtocolConfig `koanf:"protocols"`
}

// ServerConfig holds the two-plane listen addresses (ARCHITECTURE §1).
type ServerConfig struct {
	// DataPlaneAddr is the listen address for the 8-protocol data plane.
	// Consumers hit this address; no authentication is applied here.
	// Example: "0.0.0.0:7732" (see the port rationale in specula.example.yaml)
	DataPlaneAddr string `koanf:"data_plane_addr"`

	// ControlPlaneAddr is the listen address for the embedded WebUI +
	// Admin API (email-authenticated management plane). Example: "0.0.0.0:7733"
	ControlPlaneAddr string `koanf:"control_plane_addr"`

	// RegistryPublicHost is the host:port clients use to reach the OCI registry
	// (the data plane) — the value that belongs in `docker login <host>`.
	//
	// The WebUI cannot infer this: the browser is talking to the CONTROL plane,
	// and the registry answers on the data plane, which is a different port and,
	// behind an Ingress, usually a different hostname entirely. Left empty, the
	// server derives "<host the browser used>:<data plane port>", which is right
	// for a local single-binary run and wrong the moment a proxy is involved.
	// Set it explicitly for any real deployment. Example: "registry.example.com"
	RegistryPublicHost string `koanf:"registry_public_host"`

	// HA enables multi-replica mode checks: meta must be postgres, coalesce
	// lock_driver must be redis (cross-replica stampede lock via redsync), and
	// CAS must be shared — blob.driver=s3 (any S3-compatible endpoint) OR
	// blob.driver=local with local.shared=true (PVC/NFS). Production does not
	// require MinIO/AWS specifically.
	HA bool `koanf:"ha"`

	// Mode is "online" (default) or "offline". Offline serves only already-cached
	// content: upstream Fetch/Revalidate and git mirror clone/refresh are blocked
	// and cache misses return 404 (PRD US-5). Restart required to switch.
	Mode string `koanf:"mode"`
}

// Offline reports whether the server is in air-gap mode (no outbound fetches).
func (c *Config) Offline() bool {
	if c == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(c.Server.Mode), "offline")
}

// CoalesceConfig selects the cross-instance stampede lock backend
// (ARCHITECTURE §7 tier 2). Empty lock_driver means in-process only (nil
// Locker): same-replica singleflight still applies. HA requires "redis".
type CoalesceConfig struct {
	// LockDriver is "local" (nil / in-process only), "redis" (redsync), or "" (same as local).
	LockDriver string      `koanf:"lock_driver"`
	Redis      RedisConfig `koanf:"redis"`
}

// RedisConfig configures the go-redis client used by redsync (HA stampede lock).
type RedisConfig struct {
	// Addr is host:port (required when lock_driver=redis).
	Addr string `koanf:"addr"`
	// Password is optional.
	Password string `koanf:"password"`
	// DB is the Redis logical database index.
	DB int `koanf:"db"`
	// KeyPrefix is prepended to lock keys. Empty defaults to "specula:lock:".
	KeyPrefix string `koanf:"key_prefix"`
}

// StorageConfig selects the blob (CAS) and metadata backends.
type StorageConfig struct {
	Blob BlobStorageConfig `koanf:"blob"`
	Meta MetaStorageConfig `koanf:"meta"`
}

// BlobStorageConfig configures the content-addressed blob store (CAS).
// Two drivers are supported: "local" (disk-based, hard-link dedup) and
// "s3" (S3-compatible: AWS S3, MinIO, Ceph RGW, R2).
type BlobStorageConfig struct {
	// Driver is "local" or "s3".
	Driver string          `koanf:"driver"`
	Local  LocalBlobConfig `koanf:"local"`
	S3     S3BlobConfig    `koanf:"s3"`
}

// LocalBlobConfig configures the local-disk CAS driver.
type LocalBlobConfig struct {
	// Root is the directory where blobs are stored in a content-addressed
	// layout (first two hex chars of digest as subdir).
	Root string `koanf:"root"`
	// Shared must be true when server.ha is set and blob.driver is local: the
	// root must be a multi-replica shared volume (PVC/NFS), not per-pod emptyDir.
	// Specula cannot prove the mount is shared; this is the operator attestation
	// that chart/values enforce.
	Shared bool `koanf:"shared"`
}

// S3BlobConfig configures the S3-compatible CAS driver.
type S3BlobConfig struct {
	Bucket          string `koanf:"bucket"`
	Endpoint        string `koanf:"endpoint"`          // empty = AWS; set for MinIO/R2/OSS
	Region          string `koanf:"region"`            // e.g. "us-east-1" or "auto"
	AccessKeyID     string `koanf:"access_key_id"`     // empty = SDK credential chain
	SecretAccessKey string `koanf:"secret_access_key"` // empty = SDK credential chain
	UsePathStyle    bool   `koanf:"use_path_style"`    // required for MinIO
}

// MetaStorageConfig configures the metadata store (CacheEntry, MutableEntry, Users).
// Two drivers are supported: "sqlite" (single-instance, WAL mode) and
// "postgres" (HA, ON CONFLICT upsert).
type MetaStorageConfig struct {
	// Driver is "sqlite" or "postgres".
	Driver string `koanf:"driver"`
	// DSN is the data source name. For SQLite: file path (e.g. ~/.specula/meta.db).
	// For PostgreSQL: connection string (e.g. postgres://user:pass@host:5432/specula).
	DSN string `koanf:"dsn"`
}

// CacheConfig holds global cache defaults that apply to all protocols unless
// overridden per-protocol.
type CacheConfig struct {
	// DefaultMutableTTLSeconds is the fallback TTL for mutable metadata (tags,
	// index pages, packuments). Use TTLNeverRevalidate (-1) or TTLAlwaysRevalidate (0)
	// as sentinels; positive values are seconds.
	DefaultMutableTTLSeconds int64 `koanf:"default_mutable_ttl_seconds"`

	// NegativeTTLSeconds is how long to cache 404 responses (negative cache).
	// 0 disables negative caching; positive values are seconds.
	// Default matches Artifactory's 1800s (30 min) to absorb miss stampedes.
	NegativeTTLSeconds int64 `koanf:"negative_ttl_seconds"`

	// MaxBytes is the hard ceiling on total immutable cache size
	// (SUM of cache_entries.size across all protocols). 0 = unlimited
	// (default). When exceeded after a Store, Specula evicts the oldest
	// unpinned entries (and their CAS blobs) until usage is at or below
	// MaxBytes. Pinned entries are never evicted.
	MaxBytes int64 `koanf:"max_bytes"`
}

// AuthConfig configures control-plane authentication (ARCHITECTURE §11).
type AuthConfig struct {
	// JWTSecret is the HS256 signing key for session cookies. Empty means
	// auto-generated on first start and persisted into the encrypted settings
	// store (see ConfigSecret), so sessions survive restarts and every HA
	// replica shares the key. Must be kept stable across restarts or all
	// sessions are invalidated.
	//
	// This is the bootstrap default for the auth.jwt_secret runtime setting: a
	// runtime override in the encrypted store wins over this value.
	JWTSecret string `koanf:"jwt_secret"`

	// ConfigSecret is the AES-256-GCM master key (base64 of exactly 32 bytes)
	// for the encrypted runtime-settings store (internal/configstore). Generate
	// one with `openssl rand -base64 32`.
	//
	// Empty DISABLES the store: runtime settings become read-only, the admin
	// settings endpoints answer 503 on write, and any secret Specula must
	// auto-generate (jwt_secret, the registry token key) falls back to the
	// legacy ephemeral/on-disk behaviour with a loud warning. This is the
	// graceful-degradation path for dev; production should set it.
	//
	// A NON-EMPTY but malformed value is a startup ERROR, never a silent
	// downgrade to disabled: treating a typo'd key as "unset" would put secrets
	// in the database in plaintext while the operator believed encryption was on.
	//
	// Keep it OUT of the database it protects — that separation is the entire
	// point. Prefer SPECULA_AUTH__CONFIG_SECRET from a secret manager over
	// writing it into the YAML file.
	ConfigSecret string `koanf:"config_secret"`

	// CookieSecure sets the Secure flag on session cookies. Set true when
	// the control plane is behind HTTPS (recommended for production).
	CookieSecure bool `koanf:"cookie_secure"`

	// RegistryTokenKeyPath is the on-disk PEM file holding the RS256 signing
	// keypair for hosted-registry Bearer tokens (the Docker v2 token flow).
	// Distinct from JWTSecret (HS256 session cookies). Empty derives a durable
	// default next to the local blob store (or a temp path otherwise). The key
	// is generated on first start and must be kept stable across restarts /
	// shared across HA replicas so issued tokens verify everywhere.
	RegistryTokenKeyPath string `koanf:"registry_token_key_path"`
}

// ProtocolConfig holds per-protocol upstreams and verification policy.
// Keys in Config.Protocols correspond to protocol names: "oci", "pypi",
// "npm", "go", "apt", "helm", "tarball", "git", "cargo", "conda", "hf".
type ProtocolConfig struct {
	// Upstreams is the ordered fallback chain. The handler tries each
	// in ascending Priority order; lower Priority = tried first.
	Upstreams []UpstreamConfig `koanf:"upstreams"`

	// Verification configures the chain for this protocol.
	Verification VerificationConfig `koanf:"verification"`

	// MutableTTLSeconds overrides CacheConfig.DefaultMutableTTLSeconds for
	// this protocol. TTLNeverRevalidate (-1), TTLAlwaysRevalidate (0), or >0.
	//
	// It is a POINTER because 0 is a meaningful sentinel ("revalidate every
	// request"), not an absence. A plain int64 cannot distinguish "the operator
	// wrote 0" from "the operator wrote nothing" — Go's zero value collapses the
	// two — and resolution consequently discarded a documented, shipped sentinel
	// as if it were unset. nil means unset (inherit the global default); a
	// non-nil 0 is the sentinel. Use EffectiveMutableTTL to resolve it rather
	// than reading this field directly.
	MutableTTLSeconds *int64 `koanf:"mutable_ttl_seconds"`

	// SumDB configures the Go checksum-database verification + /sumdb/
	// passthrough. Only meaningful for the "go" protocol; nil for all others.
	// (PRD §6 go block; DESIGN-REVIEW H5.)
	SumDB *SumDBConfig `koanf:"sumdb"`

	// Git configures the git-clone acceleration handler (bare-mirror model).
	// Only meaningful for the "git" protocol; nil for all others (PRD §6 git
	// block; ARCHITECTURE §9). git does not use the generic Upstreams fallback
	// chain — it reverse-proxies / mirrors the AllowedUpstreams hosts directly.
	Git *GitConfig `koanf:"git"`

	// Cargo configures Cargo sparse-registry extras (crate download mirrors).
	// Index upstreams use the generic Upstreams list; dl_upstreams fetch .crate
	// files (default: https://static.crates.io when omitted).
	Cargo *CargoConfig `koanf:"cargo"`

	// OCI configures OCI-specific pull-through extras (multi-registry allowlist).
	// Only meaningful for the "oci" protocol; nil for all others.
	OCI *OCIConfig `koanf:"oci"`

	// Apt configures APT multi-archive allowlist (/apt/<archive>/dists|pool/…).
	// Only meaningful for the "apt" protocol; nil for all others.
	Apt *AptConfig `koanf:"apt"`

	// Helm configures classic-HTTP multi-repo allowlist (/helm/<repo>/…).
	// Only meaningful for the "helm" protocol; nil for all others.
	Helm *HelmConfig `koanf:"helm"`

	// Conda configures per-channel root allowlist (/conda/<channel>/…).
	// Only meaningful for the "conda" protocol; nil for all others.
	Conda *CondaConfig `koanf:"conda"`
}

// NamedSource is a named upstream root used by apt/helm/conda multi-source
// allowlists (archive / repo / channel).
type NamedSource struct {
	Name    string `koanf:"name"`
	BaseURL string `koanf:"base_url"`
}

// AptConfig holds APT multi-archive settings.
// Empty Repositories → legacy behavior: any path prefix is cache-scoped only;
// fetch always uses protocols.apt.upstreams (archive root).
// Non-empty → /apt/<name>/… must match an allowlisted archive; unknown → 404.
type AptConfig struct {
	Repositories []NamedSource `koanf:"repositories"`
}

// HelmConfig holds classic-HTTP Helm multi-repo settings.
// Empty Repositories → legacy: repo segment is a subpath under upstreams BaseURL.
// Non-empty → /helm/<name>/… routes to that repo's BaseURL with path strip.
type HelmConfig struct {
	Repositories []NamedSource `koanf:"repositories"`
}

// CondaConfig holds per-channel root settings.
// Empty Channels → legacy: full path under cloud-root upstreams.
// Non-empty → /conda/<name>/… routes to that channel BaseURL after stripping name.
type CondaConfig struct {
	Channels []NamedSource `koanf:"channels"`
}

// CargoConfig holds Cargo sparse-registry extras (download mirrors + multi-registry allowlist).
// Empty Registries → legacy behavior: full index path under protocols.cargo.upstreams.
// Non-empty → /cargo/index/<name>/… routes to that registry's BaseURL after stripping name.
type CargoConfig struct {
	// DLUpstreams are ordered mirrors for .crate downloads (static.crates.io
	// layout). Empty → handler default https://static.crates.io.
	DLUpstreams []UpstreamConfig `koanf:"dl_upstreams"`

	// Registries is the allowlist of named sparse-index roots for path-style
	// multi-registry pulls (/cargo/index/<name>/…). Unknown names → 404.
	Registries []NamedSource `koanf:"registries"`
}

// OCIConfig holds OCI multi-registry pull-through settings.
// Unqualified names (library/nginx) still use protocols.oci.upstreams (Hub chain).
// Names prefixed with an allowlisted host (codeberg.org/org/repo) route to that
// registry after stripping the host from the upstream path.
type OCIConfig struct {
	// RemoteRegistries is the SSRF allowlist of non-Hub registries Specula will
	// proxy. Empty → multi-registry path-style pulls are rejected (404).
	RemoteRegistries []OCIRemoteRegistry `koanf:"remote_registries"`
}

// OCIRemoteRegistry is one allowlisted remote OCI registry host.
type OCIRemoteRegistry struct {
	// Host is the registry hostname as it appears in image refs / path prefixes
	// (e.g. "ghcr.io", "codeberg.org"). Compared case-insensitively.
	Host string `koanf:"host"`

	// BaseURL overrides the upstream root (default: https://<host>).
	BaseURL string `koanf:"base_url"`
}

// GitConfig holds the git-clone acceleration settings for the "git" protocol
// (PRD §6, ARCHITECTURE §9). The handler keeps a disk bare-mirror cache (git
// objects are content-addressed by SHA = immutable; refs = mutable short TTL)
// and passes through push / authenticated / private requests untouched.
type GitConfig struct {
	// AllowedUpstreams is the host allowlist (e.g. ["github.com", "gitlab.com"]).
	// A request whose host is not listed is rejected (404) — never proxied.
	AllowedUpstreams []string `koanf:"allowed_upstreams"`

	// MirrorDir is the on-disk root for bare mirrors (git objects live here,
	// content-addressed by SHA). Example: /var/specula/git.
	MirrorDir string `koanf:"mirror_dir"`

	// SyncStaleAfter is the staleness window for a bare mirror before a
	// `git remote update` is triggered on the next request (Go duration string,
	// e.g. "30s"). Concurrent clones within the window reuse the same fetch.
	SyncStaleAfter string `koanf:"sync_stale_after"`

	// PublicOnly, when true, restricts caching to anonymously-readable repos.
	// Private repos / requests bearing Authorization are passed through with
	// zero caching (never mirrored). Recommended: true.
	PublicOnly bool `koanf:"public_only"`

	// FailClosed, when true, passes a request through (bypass) rather than
	// serving from a stale mirror when the public-visibility probe fails — the
	// probe-failure window is exactly when an attacker's public copy could win.
	FailClosed bool `koanf:"fail_closed"`
}

// SumDBConfig is the Go checksum-database (sumdb) configuration surface for the
// "go" protocol (PRD §6). The proxy verifies module authenticity against a
// signed sumdb tree head routed via a CN-reachable passthrough, and refuses to
// forward private module names to the public sumdb.
//
// Trust rule (DESIGN-REVIEW H5): GOSUMDB is NEVER defaulted to "off". Policy is
// "enforce" (default) or "warn"; anything else — including "off" — is rejected
// by Validate.
type SumDBConfig struct {
	// URL is the sumdb access endpoint, in either of two wire shapes. Both the
	// chain verifier and the /sumdb/ passthrough resolve it through
	// verify.SumDBEndpoint, so both shapes work for both (see that type):
	//
	//   DIRECT — the checksum database at its host root; the name is NOT part of
	//     its URL space.  e.g. "https://sum.golang.google.cn"  (CN default)
	//   PROXY  — a GOPROXY module-proxy base whose path ends in "/sumdb"; it
	//     routes on "/<sumdb-name>/...".  e.g. "https://goproxy.cn/sumdb"
	//
	// Empty falls back to the compiled default (https://sum.golang.org, direct)
	// — acceptable only where it is reachable, which in CN it is not.
	URL string `koanf:"url"`

	// Policy is "enforce" (fail closed on verification failure) or "warn"
	// (log + serve, degraded tier). Empty defaults to "enforce". "off" is
	// explicitly rejected — never disable sumdb verification globally.
	Policy string `koanf:"policy"`

	// PrivatePatterns are GONOSUMDB-style globs (Athens NoSumPatterns). Module
	// paths matching any glob are treated as private: their names are NEVER
	// forwarded to the public sumdb and /sumdb/ lookups for them return 403.
	// Example: ["git.internal.corp/*", "*.corp.example.com/*"].
	PrivatePatterns []string `koanf:"private_patterns"`

	// RollbackToleranceEntries bounds how far a signed tree head may regress
	// below the persisted anti-rollback high-water mark before it is treated as
	// an attack rather than CDN edge lag.
	//
	// nil (omitted) uses the built-in default (5000 entries ≈ 1–5.5h of log
	// growth). 0 is strict: any regression at all fails closed. A regression
	// within the window is WARN-logged and never advances the high-water mark.
	//
	// Rationale, measurements and threat analysis: verify.defaultRollbackTolerance
	// Entries in internal/verify/sumdb_client.go. In short: sum.golang.google.cn
	// serves /latest with `cache-control: max-age=300`, so a lagging CDN edge
	// legitimately returns an older head; strict mode intermittently bricks CN
	// `go get`.
	RollbackToleranceEntries *int64 `koanf:"rollback_tolerance_entries"`

	// VerifierKey pins the sumdb note verifier key ("<name>+<hash>+<base64key>",
	// golang.org/x/mod/sumdb/note format). Empty uses the default sum.golang.org
	// key embedded in x/mod. Setting it enables explicit key pinning.
	VerifierKey string `koanf:"verifier_key"`
}

// UpstreamConfig describes one mirror in the fallback chain for a protocol.
type UpstreamConfig struct {
	// Name is a human-readable identifier used in logs and metrics.
	Name string `koanf:"name"`

	// BaseURL is the root URL for this upstream (no trailing slash).
	// Example: "https://registry-1.docker.io"
	BaseURL string `koanf:"base_url"`

	// Priority controls fallback order. Lower = higher priority (tried first).
	Priority int `koanf:"priority"`

	// Official marks this upstream as the authoritative source. Used by the
	// consensus verifier as the "origin-check" witness.
	Official bool `koanf:"official"`
}

// VerificationConfig configures the verification chain for one protocol
// (ARCHITECTURE §5, DESIGN-REVIEW §1.2).
type VerificationConfig struct {
	// Tiers lists which verification tiers to run for this protocol.
	// Valid values: "checksum", "tofu", "consensus", "signed".
	// The chain runs in ascending trust order; the highest tier reached
	// is recorded in the CacheEntry.
	Tiers []string `koanf:"tiers"`

	// Quorum is the minimum number of independent upstreams that must
	// agree on a digest for the "consensus" tier to pass. Must be >= 1
	// when "consensus" is in Tiers. Superseded by Consensus.Quorum when the
	// structured Consensus block is present; retained for back-compat.
	Quorum int `koanf:"quorum"`

	// Consensus configures the cross-source consensus tier (TierConsensus):
	// independent mirrors polled for a digest + optional official-source
	// witness. nil disables it. (DESIGN-REVIEW §1.2 cross-source consensus.)
	Consensus *ConsensusConfig `koanf:"consensus"`

	// CosignKey is the path to a cosign public key (PEM format) for
	// keyed OCI image verification (--insecure-ignore-tlog). Superseded by the
	// structured Cosign block when present; retained for back-compat.
	CosignKey string `koanf:"cosign_key"`

	// Cosign configures keyed cosign OCI image verification with the
	// transparency log disabled (CN-offline). Only meaningful for "oci"; nil
	// disables it. (DESIGN-REVIEW §1.1 cosign keyed anchor.)
	Cosign *CosignConfig `koanf:"cosign"`

	// Keyring is the path to a GPG keyring for apt InRelease / Helm .prov
	// signature verification.
	Keyring string `koanf:"keyring"`

	// AllowedSigners is the path to a git allowed-signers file for
	// verifying signed tags/commits.
	AllowedSigners string `koanf:"allowed_signers"`

	// SumDBKey is the Go checksum database note verifier key
	// (golang.org/x/mod/sumdb/note format). Defaults to the Go module
	// proxy key if empty; explicitly setting it enables pinning.
	SumDBKey string `koanf:"sumdb_key"`

	// GPG configures the apt end-to-end GPG chain verifier (InRelease →
	// Packages → .deb) against a local keyring. Only meaningful for "apt".
	// (PRD §6 apt block; DESIGN-REVIEW §1.1 apt gold standard.)
	GPG *GPGConfig `koanf:"gpg"`

	// Provenance configures the Helm .prov detached-GPG-signature verifier
	// against a local keyring. Only meaningful for the classic-HTTP "helm"
	// repo. (PRD §6 helm block.)
	Provenance *ProvenanceConfig `koanf:"provenance"`

	// SignedRefs configures the git signed tag/commit verifier against an
	// allowed-signers file. Only meaningful for "git". (PRD §6 git block.)
	SignedRefs *SignedRefsConfig `koanf:"signed_refs"`

	// Tofu is the TOFU (trust-on-first-use) policy for this protocol:
	// "enforce" (fail closed on a digest change for an immutable version) or
	// "warn" (alert only). Empty leaves TOFU governed solely by Tiers. This is
	// the primary anchor for pypi/npm/tarball (PRD §信任模型).
	Tofu string `koanf:"tofu"`

	// DependencyConfusion configures the private-namespace / fail-closed guard
	// for flat-or-scoped ecosystems (pypi, npm). nil disables it. (PRD §6
	// pypi/npm blocks; DESIGN-REVIEW §4.)
	DependencyConfusion *DependencyConfusionConfig `koanf:"dependency_confusion"`

	// Maturity configures the cool-down / min-age policy gate for package
	// ecosystems that advertise a publish time (npm, pypi, cargo). nil disables
	// it. This is NOT a trust tier — it is a structural hold on young versions
	// (PRD v0.10; docs/TRUST.md §5).
	Maturity *MaturityConfig `koanf:"maturity"`
}

// MaturityConfig holds the cool-down policy for one protocol.
type MaturityConfig struct {
	// MinAge is how old a version must be before it may pass the gate
	// (Go duration string, e.g. "72h", "168h"). Empty / zero disables the gate.
	MinAge string `koanf:"min_age"`

	// Policy is "warn" (StatusWarn, still cached) or "enforce" (StatusFail,
	// quarantine discarded). Empty defaults to "warn".
	Policy string `koanf:"policy"`
}

// ConsensusConfig is the cross-source consensus block (DESIGN-REVIEW §1.2). It
// polls independent mirrors for a digest/manifest (HEAD/metadata only, never the
// full blob) and passes when >= Quorum agree. An optional official-source
// witness fails closed on disagreement.
type ConsensusConfig struct {
	// Quorum is the minimum number of independent mirrors that must agree on the
	// artifact digest for a PASS. Must be >= 1.
	Quorum int `koanf:"quorum"`
	// Mirrors is the set of independent (distinct CDN/operator) mirrors to poll.
	Mirrors []ConsensusMirrorConfig `koanf:"mirrors"`
	// OriginCheck optionally consults the official source directly (through an
	// egress proxy) as an authoritative witness. nil disables it.
	OriginCheck *OriginCheckConfig `koanf:"origin_check"`
}

// ConsensusMirrorConfig is one independent mirror consulted for a digest.
type ConsensusMirrorConfig struct {
	// Name is a logical identifier used in logs/messages.
	Name string `koanf:"name"`
	// BaseURL is the mirror base URL.
	BaseURL string `koanf:"base_url"`
}

// OriginCheckConfig configures the authoritative official-source witness.
type OriginCheckConfig struct {
	// URL is the official source base (pypi.org / registry.npmjs.org /
	// registry-1.docker.io). Empty disables the origin check.
	URL string `koanf:"url"`
	// ViaProxy is the egress HTTP proxy URL used to reach the official source
	// from a restricted network. Empty means reach it directly.
	ViaProxy string `koanf:"via_proxy"`
}

// CosignConfig is the keyed-cosign OCI verification block with the transparency
// log disabled (DESIGN-REVIEW §1.1). Keyless/tlog verification is unsupported
// (Rekor/Fulcio are CN-blocked), so Tlog must be false.
type CosignConfig struct {
	// Keys are filesystem paths to long-lived cosign PUBLIC keys (PEM). A
	// signature verifying against any listed key passes (supports key rotation).
	Keys []string `koanf:"keys"`
	// Tlog MUST be false: keyless/transparency-log verification is unsupported.
	// Validate rejects a true value.
	Tlog bool `koanf:"tlog"`
}

// GPGConfig is the apt GPG chain-verification block (PRD §6 apt).
type GPGConfig struct {
	// Policy is "enforce" (fail closed on a broken chain) or "warn".
	// Empty defaults to "enforce" — apt is a full signed anchor.
	Policy string `koanf:"policy"`
	// Keyring is the path to the local, out-of-band distro keyring
	// (e.g. /etc/specula/ubuntu-archive-keyring.gpg). A mirror cannot forge it.
	Keyring string `koanf:"keyring"`
}

// ProvenanceConfig is the Helm .prov GPG-verification block (PRD §6 helm).
type ProvenanceConfig struct {
	// Policy is "enforce" or "warn". Empty defaults to "warn": a chart without
	// a .prov attachment degrades to a lower tier rather than failing.
	Policy string `koanf:"policy"`
	// Keyring is the path to the local Helm signing keyring
	// (e.g. /etc/specula/helm-keyring.gpg).
	Keyring string `koanf:"keyring"`
}

// SignedRefsConfig is the git signed tag/commit block (PRD §6 git).
type SignedRefsConfig struct {
	// Policy is "enforce" or "warn". Empty defaults to "warn": signed refs are
	// opt-in, and an unsigned ref degrades to tofu rather than failing.
	Policy string `koanf:"policy"`
	// AllowedSigners is the path to the git allowed-signers file (SSH format)
	// used by `git verify-tag` / `git verify-commit`.
	AllowedSigners string `koanf:"allowed_signers"`
}

// DependencyConfusionConfig is the private-namespace guard (DESIGN-REVIEW §4).
// Private names resolve ONLY from the private upstream; on private-upstream
// failure the guard fails closed (never falls back to a public mirror).
type DependencyConfusionConfig struct {
	// PrivateNames is an EXACT list of private package names/patterns for flat
	// ecosystems (pypi). Prefix "conventions" are security theatre — list the
	// names the org actually owns (e.g. ["mycompany-*"]).
	PrivateNames []string `koanf:"private_names"`
	// PrivateScopes is the list of npm scopes bound to the private registry
	// (e.g. ["@myorg"]). Scoped names are structurally confusion-resistant.
	PrivateScopes []string `koanf:"private_scopes"`
	// PrivateUnscoped is the explicit denylist of unscoped npm names that must
	// never be queried upstream (e.g. ["internal-svc"]).
	PrivateUnscoped []string `koanf:"private_unscoped"`
	// PrivateUpstream is the base URL of the private registry/index that owns
	// the names above.
	PrivateUpstream string `koanf:"private_upstream"`
	// OnPrivateDown selects behaviour when the private upstream is unreachable:
	// "fail_closed" (default; 5xx, never fall back to public) or
	// "serve_stale" (serve from local cache only). Empty = "fail_closed".
	OnPrivateDown string `koanf:"on_private_down"`
}

// Load reads and parses the YAML config file at path, applies SPECULA_*
// environment variable overrides (highest precedence), validates the result,
// and returns the populated Config.
//
// Validation is fail-fast: all detected errors are joined into a single
// error value with clear field paths.
//
// Environment override format:
//
//	SPECULA_<LEVEL>__<LEVEL>__<KEY>
//
// Examples:
//
//	SPECULA_SERVER__DATA_PLANE_ADDR=0.0.0.0:7732
//	SPECULA_STORAGE__BLOB__DRIVER=s3
//	SPECULA_PROTOCOLS__OCI__MUTABLE_TTL_SECONDS=300
func Load(path string) (*Config, error) {
	k := koanf.New(".")

	// Layer 0: built-in defaults. These are the product's ports, not a suggestion
	// living in an example file — a config that omits them must still start, and
	// start on Specula's own ports rather than refusing to boot.
	if err := k.Load(confmap.Provider(defaults(), "."), nil); err != nil {
		return nil, fmt.Errorf("config: load defaults: %w", err)
	}

	// Layer 1: YAML file (base configuration).
	if err := k.Load(file.Provider(path), yaml.Parser()); err != nil {
		return nil, fmt.Errorf("config: load file %q: %w", path, err)
	}

	// Layer 2: Environment variable overrides.
	// Provider returns nil, nil from ReadBytes so koanf calls Read().
	if err := k.Load(newEnvProvider(EnvPrefix), nil); err != nil {
		return nil, fmt.Errorf("config: load env: %w", err)
	}

	// Unmarshal with ErrorUnused: true so that misplaced or misspelled keys are
	// a hard error rather than a silent no-op.  A typo that silently disables a
	// security anchor (e.g. sumdb placed under verification instead of at the
	// protocol level) is the worst possible failure mode for a product whose
	// thesis is honest supply-chain verification.
	//
	// We preserve koanf's default WeaklyTypedInput (required so that SPECULA_*
	// string env vars can coerce to int64/bool at unmarshal time) and the two
	// standard decode hooks (StringToTimeDuration for future use, TextUnmarshaller
	// for types that implement encoding.TextUnmarshaler).
	var cfg Config
	dc := &mapstructure.DecoderConfig{
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.TextUnmarshallerHookFunc(),
		),
		WeaklyTypedInput: true,
		ErrorUnused:      true,
	}
	if err := k.UnmarshalWithConf("", &cfg, koanf.UnmarshalConf{DecoderConfig: dc}); err != nil {
		return nil, fmt.Errorf("config: unmarshal: %w", err)
	}

	// Expand ~/… paths, then fill any still-empty local storage fields from
	// $HOME/.specula so a config that omits storage still boots without root.
	if err := expandConfigPaths(&cfg); err != nil {
		return nil, err
	}
	if err := applyStorageDefaults(&cfg); err != nil {
		return nil, err
	}

	if err := Validate(&cfg); err != nil {
		return nil, err
	}

	return &cfg, nil
}

// Default listen addresses. These are Specula's ports, baked into the binary —
// a config that omits them starts here rather than failing to boot.
//
// 7732/7733 spell "SPEC" on a phone keypad (S=7 P=7 E=3 C=2): Specula, and the
// specs it exists to conform to. They are deliberately not 5000/8080. Port 5000
// is the Docker registry / zot default — the single most likely thing to already
// be listening on a host that wants an OCI cache — and 8080 needs no
// explanation; on the development host both were already taken, as was 9090.
// A collision here is not a cosmetic problem: it has already caused a
// conformance run to silently grade a different server.
const (
	DefaultDataPlaneAddr    = "0.0.0.0:7732"
	DefaultControlPlaneAddr = "0.0.0.0:7733"
)

// TTLPtr returns a pointer to v, for building ProtocolConfig literals in code
// and tests: `MutableTTLSeconds: TTLPtr(TTLAlwaysRevalidate)` states "this
// protocol explicitly sets the always-revalidate sentinel", which a bare int64
// field could not express distinctly from "unset".
func TTLPtr(v int64) *int64 { return &v }

// EffectiveMutableTTL resolves the mutable-metadata TTL that actually applies to
// a protocol: the protocol's own value when it set one, otherwise the global
// CacheConfig.DefaultMutableTTLSeconds.
//
// The sentinels (ARCHITECTURE §3) are part of the value space, not markers of
// absence:
//
//	-1 (TTLNeverRevalidate)  = never revalidate
//	 0 (TTLAlwaysRevalidate) = revalidate on every request
//	>0                       = seconds
//
// Absence is carried by the pointer being nil, which is the whole reason
// ProtocolConfig.MutableTTLSeconds is a pointer. This is the ONLY place that
// distinction should be interpreted; callers take the resolved int64.
func (c *Config) EffectiveMutableTTL(pc ProtocolConfig) int64 {
	if pc.MutableTTLSeconds != nil {
		return *pc.MutableTTLSeconds
	}
	return c.Cache.DefaultMutableTTLSeconds
}

// defaults returns the built-in configuration, applied beneath the YAML file and
// environment overrides.
func defaults() map[string]any {
	return map[string]any{
		"server.data_plane_addr":    DefaultDataPlaneAddr,
		"server.control_plane_addr": DefaultControlPlaneAddr,
	}
}
