package oci

import (
	"context"
	"errors"
)

// HostedResolver is the hosted-first pull seam (REGISTRY-DESIGN §0/§1). Before
// the pull-through handler falls back to an upstream fetch, it asks the resolver
// whether the requested repository name is an org-owned HOSTED repo (a repo that
// was pushed to Specula, not merely a mirror of an upstream image).
//
// Resolution outcomes:
//   - hosted=true  → the repo is authoritative local content: it is served from
//     the shared CAS (subject to the resolver/authz layer's acl visibility
//     check) and MUST NOT be fetched from any upstream. Hosted CacheEntry rows
//     carry origin=hosted and are never GC-evicted (they are data, not cache).
//   - hosted=false → not a hosted repo; the handler falls through to the normal
//     upstream pull-through cache path unchanged.
//
// The interface is intentionally narrow so oci has no dependency on the repo /
// acl packages: the concrete implementation (wired in R2) lives outside this
// package and closes over repo.RepoStore + acl.CanAccess.
type HostedResolver interface {
	// ResolveHosted reports whether name ("<org>/<repo>") is a hosted repo.
	// A non-nil error is treated as "unknown" by callers, which then fall back
	// to the upstream path (fail-open to preserve pull availability).
	ResolveHosted(ctx context.Context, name string) (hosted bool, err error)
}

// WithHostedResolver injects the hosted-first resolver. When unset (the default
// pull-through wiring) the seam is inert and pull behaviour is unchanged.
func WithHostedResolver(r HostedResolver) Option {
	return func(h *Handler) { h.hosted = r }
}

// isHosted consults the hosted resolver, returning false when no resolver is
// configured or the lookup errors (fail-open to the upstream pull path). This is
// the single call site the manifest/blob serve paths use to check "is this a
// hosted repo?" before considering an upstream fetch.
func (h *Handler) isHosted(ctx context.Context, name string) bool {
	if h.hosted == nil {
		return false
	}
	hosted, err := h.hosted.ResolveHosted(ctx, name)
	if err != nil {
		h.log.Warn("oci: hosted resolver error — falling back to upstream", "name", name, "err", err)
		return false
	}
	return hosted
}

// ─── Hosted visibility enforcement ──────────────────────────────────────────

// HostedReadAuthz enforces per-repo visibility for hosted repos. It is called
// after isHosted confirms a repo is hosted and before any CAS access, so that
// visibility is enforced for both cache-hit and cache-miss paths.
//
// The concrete implementation (wired in R2 by internal/registry or cmd/specula)
// closes over repo.RepoStore + acl.CanAccess and extracts the caller's acl.Subject
// from the request context (where the registry-token Bearer middleware parks JWT
// claims). This seam keeps the oci package free of repo / acl / registrytoken
// imports.
type HostedReadAuthz interface {
	// AuthorizeRead returns nil when the caller may pull from repoName.
	// Return ErrUnauthorized when no valid token is present (handler emits 401).
	// Return any other error — including ErrForbidden — when a token is present
	// but its scope is insufficient (handler emits 403).
	AuthorizeRead(ctx context.Context, repoName string) error
}

var (
	// ErrUnauthorized signals that the request carries no valid authentication
	// token. The handler maps this to HTTP 401 with an UNAUTHORIZED error body.
	ErrUnauthorized = errors.New("oci: unauthorized — authentication required")

	// ErrForbidden signals that a valid token is present but does not cover a
	// pull on this repo. The handler maps this to HTTP 403 with a DENIED body.
	ErrForbidden = errors.New("oci: forbidden — insufficient scope")
)

// WithHostedReadAuthz injects the visibility-enforcement authorizer for hosted
// repos. When unset, all callers may read hosted repos without an access check
// (fail-open; the outer middleware layer is expected to enforce auth in
// production).
func WithHostedReadAuthz(a HostedReadAuthz) Option {
	return func(h *Handler) { h.hostedAuthz = a }
}

// checkHostedRead delegates to hostedAuthz.AuthorizeRead when an authorizer is
// configured. Returns nil (allow) when no authorizer is wired.
func (h *Handler) checkHostedRead(ctx context.Context, name string) error {
	if h.hostedAuthz == nil {
		return nil
	}
	return h.hostedAuthz.AuthorizeRead(ctx, name)
}

// ─── Owned-namespace gate ────────────────────────────────────────────────────

// OwnedNamespaceResolver reports whether a repository name belongs to a
// Specula-owned namespace (a known org), independent of whether the specific
// repo has been created yet. Content under an owned namespace is authoritative-
// local: a cache miss must return 404 and MUST NOT fall through to a configured
// upstream mirror. Without this gate a push into an org namespace breaks the
// moment an OCI pull-through upstream is configured — docker's HEAD-blob
// existence check for a not-yet-created repo would leak the org name to the
// public upstream (and return 502/403 instead of the 404 that lets the upload
// proceed). A name outside every owned namespace (e.g. "library/nginx") is a
// pull-through mirror name and keeps the existing upstream fallback.
//
// The interface is intentionally narrow so oci keeps no dependency on the repo /
// org / acl packages; the concrete implementation (wired in R2) closes over the
// org store.
type OwnedNamespaceResolver interface {
	// IsOwnedNamespace reports whether name ("<org>/<repo>") sits under a known
	// org namespace. A non-nil error is treated as "not owned" by callers, which
	// then preserve the upstream pull-through path (fail-open to availability).
	IsOwnedNamespace(ctx context.Context, name string) (owned bool, err error)
}

// WithOwnedNamespaceResolver injects the owned-namespace gate. When unset the
// gate is inert and upstream fallback behaviour is unchanged.
func WithOwnedNamespaceResolver(r OwnedNamespaceResolver) Option {
	return func(h *Handler) { h.owned = r }
}

// isOwnedNamespace consults the owned-namespace resolver, returning false when
// no resolver is configured or the lookup errors (fail-open to the upstream pull
// path). Callers use it on a cache miss to decide whether an upstream fetch is
// permitted for the name.
func (h *Handler) isOwnedNamespace(ctx context.Context, name string) bool {
	if h.owned == nil {
		return false
	}
	owned, err := h.owned.IsOwnedNamespace(ctx, name)
	if err != nil {
		h.log.Warn("oci: owned-namespace resolver error — allowing upstream fallback", "name", name, "err", err)
		return false
	}
	return owned
}
