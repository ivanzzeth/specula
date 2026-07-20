// Package artifact defines the canonical, protocol-agnostic core types shared
// across every Specula subsystem: the artifact reference, cache entries
// (immutable + mutable tiers), the honest tiered-trust enum, and the streaming
// verification result types.
//
// This is the public FOUNDATION contract (see docs/LIBRARY.md). It is
// dependency-free. Prefer this package over the internal/artifact shim.
package artifact

import "time"

// Tier is the honest tiered-trust level (DESIGN-REVIEW §1.2). Ordered from
// weakest to strongest guarantee. Never claim a higher tier than actually
// achieved for a given protocol/artifact.
type Tier int

const (
	// TierChecksum only proves transport integrity against a reference value
	// that may itself come from the mirror. NEVER a standalone supply-chain
	// control.
	TierChecksum Tier = iota
	// TierTofu locks the digest on first fetch and alerts on later change
	// (force-push / history rewrite / version rewrite detection).
	TierTofu
	// TierConsensus cross-checks the digest/manifest across N independent
	// mirrors (and optionally the official origin) — quorum agreement, not
	// cryptographic authenticity.
	TierConsensus
	// TierSigned is anchored in a cryptographic trust root obtained
	// out-of-band (apt keyring / Go sumdb / Helm .prov / cosign keyed).
	// Highest tier; resists origin forgery.
	TierSigned
)

// String returns the lowercase canonical name of the tier.
func (t Tier) String() string {
	switch t {
	case TierSigned:
		return "signed"
	case TierConsensus:
		return "consensus"
	case TierTofu:
		return "tofu"
	case TierChecksum:
		return "checksum"
	default:
		return "unknown"
	}
}

// Status is the outcome of a single verifier or of the whole chain.
type Status int

const (
	// StatusPass means the verifier accepted the artifact at its tier.
	StatusPass Status = iota
	// StatusWarn means non-fatal concern (e.g. TOFU first-lock, degraded tier).
	StatusWarn
	// StatusFail means the artifact must not be promoted or served.
	StatusFail
	// StatusSkip means the verifier DID NOT RUN for this ref — it self-gated
	// out because the artifact is not its business (wrong protocol, a mutable
	// ref, no resolved digest, a signature attachment rather than the artifact).
	//
	// Skip is not a weak pass. Before this variant existed every self-gate
	// returned StatusPass at TierChecksum, which made "the gpg verifier examined
	// this .deb and it was fine" and "the gpg verifier has nothing to do with
	// this npm tarball" the same value. That conflation is invisible while the
	// only consumer is the Chain — which aggregates by MAX(tier), where a
	// checksum-tier pass is the identity element and changes nothing — but it
	// becomes an active lie the moment anything reports per-verifier outcomes:
	// specula_verification_total would have claimed that the gpg check passed on
	// every npm package Specula ever served (PRD §G2 — never claim a check you
	// did not earn).
	//
	// Chain treats StatusSkip exactly as it treated the old no-op pass: it does
	// not fail, does not warn, and does not advance the tier. The difference is
	// that it is now sayable.
	StatusSkip
)

// String returns the lowercase canonical name of the status.
func (s Status) String() string {
	switch s {
	case StatusPass:
		return "pass"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusSkip:
		return "skip"
	default:
		return "unknown"
	}
}

// ArtifactRef is the canonical internal identity of a request, produced by the
// resolver/policy layer and consumed by cache, upstream and verify. For mutable
// requests (tags, indexes, refs) Digest is empty until resolved.
type ArtifactRef struct {
	Protocol string // oci | pypi | npm | gomod | apt | helm | tarball | git
	Name     string // image / package / module path / repo host+path
	Version  string // tag / version / suite+component / ref
	Digest   string // sha256:...; filled after resolution; the CAS key
	Upstream string // originating upstream (M4: recorded to detect cross-source conflicts)
	Mutable  bool   // tag/index/ref -> true => routed through the mutable tier
}

// UpstreamMeta carries the revalidation and provenance metadata returned by an
// upstream fetch. Signature/provenance attachments are referenced by the
// streaming verifiers (never fully buffered for large blobs).
type UpstreamMeta struct {
	ETag         string   // entity tag for conditional GET (If-None-Match)
	LastModified string   // Last-Modified for If-Modified-Since
	Upstream     string   // which upstream actually served the bytes
	ContentType  string   // reported content type
	StatusCode   int      // upstream HTTP status (e.g. 200, 304)
	Attachments  [][]byte // optional signature / provenance / .prov blobs
}

// Artifact is a quarantined, on-disk artifact handed to the verification chain.
// The bytes live at Path and are NEVER fully buffered in memory (fix C3): the
// digest is computed while streaming to disk, and signature verification runs
// against the file handle.
type Artifact struct {
	Path   string       // quarantine file path (not resident in memory)
	Digest string       // sha256:... computed while writing
	Size   int64        // byte length
	Meta   UpstreamMeta // ETag, Last-Modified, source upstream, attachments
}

// Result is the outcome of a verifier (or the chain), recording the tier
// actually achieved.
type Result struct {
	Status  Status // pass | warn | fail
	Tier    Tier   // tier actually reached
	Message string // human-readable detail (verifier name, reason)
}

// CacheEntry is the authoritative record for an immutable, verified artifact in
// the CAS layer. Size is recorded at write time so per-protocol capacity is an
// O(1) SUM (G7), never an FS walk.
type CacheEntry struct {
	Ref        ArtifactRef // canonical reference this entry answers
	Digest     string      // sha256:...; CAS key
	Size       int64       // byte length (recorded at Put)
	Protocol   string      // owning protocol (for GROUP BY aggregation)
	Tier       Tier        // tier actually achieved at verification time
	Upstream   string      // upstream the bytes came from
	ETag       string      // upstream ETag at fetch time
	VerifiedAt time.Time   // when verification passed
	CreatedAt  time.Time   // when the entry was first written
}

// MutableEntry is the short-TTL record for the mutable tier: tag->digest maps,
// OCI manifests-by-tag, /simple/ pages, packuments, index.yaml, refs. It carries
// conditional-revalidation state (fix H1).
//
// TTLSeconds uses config sentinels (stolen from Nexus): -1 = never revalidate
// (treat as immutable), 0 = revalidate every request.
type MutableEntry struct {
	Key          string    // protocol-scoped cache key (e.g. "oci:nginx:latest")
	Protocol     string    // owning protocol
	Digest       string    // resolved immutable digest for tag->digest entries
	Payload      []byte    // cached mutable body (index/packument) when applicable
	ETag         string    // upstream ETag for If-None-Match
	LastModified string    // upstream Last-Modified for If-Modified-Since
	TTLSeconds   int64     // -1 never revalidate, 0 always revalidate, >0 seconds
	Upstream     string    // upstream that produced this entry
	FetchedAt    time.Time // when the entry was (re)validated
}

// SizeStat is a per-protocol capacity aggregate produced by
// MetadataStore.CacheSizeByProtocol / stats.Collector (G7).
type SizeStat struct {
	Bytes   int64     // SUM(size)
	Objects int64     // COUNT(*) — meaningful only when ObjectsCountable is true
	Oldest  time.Time // MIN(created_at)
	Newest  time.Time // MAX(created_at)

	// ObjectsCountable reports whether Objects is a real count.
	//
	// It is false for OPAQUE caches — protocols whose bytes are measured by
	// walking a directory rather than by counting CAS/metadata rows. git is the
	// case in point: its objects live inside packfiles in a bare mirror, so the
	// collector can size the tree but cannot count objects in it.
	//
	// When false, Objects is 0 because it is UNKNOWN, not because the cache is
	// empty (Bytes proves otherwise). Consumers must render "—" / null rather
	// than a fabricated zero — the same honesty rule dto.go states for
	// UpstreamHealth's companion flags. The Prometheus gauge
	// specula_cache_objects deliberately emits no series for such protocols:
	// absent is how a Prometheus gauge says "not applicable".
	ObjectsCountable bool
}
