package npm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// staler is an optional extension of CacheManager for serve-stale-on-upstream-failure.
// The production manager implements this; test fakes may not.
type staler interface {
	LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)
}

const (
	// contentTypePackument is the Content-Type for npm packument (metadata JSON).
	contentTypePackument = "application/json"
	// contentTypeTarball is the Content-Type for npm package tarballs.
	contentTypeTarball = "application/octet-stream"
)

// npmMutableKey returns the MetadataStore key for a packument MutableEntry.
// Mirrors cache.mutableKey (unexported): protocol + ":" + name + ":" + version.
func npmMutableKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

// --------------------------------------------------------------------------
// Dependency-confusion guard (DESIGN-REVIEW §4)
// --------------------------------------------------------------------------

// isPrivatePkg reports whether pkg must be routed exclusively to the private
// upstream (DESIGN-REVIEW §4). The dependency-confusion guard is the authoritative
// source when wired; the inline logic is a backward-compat fallback.
func (h *Handler) isPrivatePkg(pkg string) bool {
	if h.guard != nil {
		return h.guard.IsPrivate(pkg)
	}
	if strings.HasPrefix(pkg, "@") {
		// Scoped: "@scope/name" — extract the scope portion.
		if i := strings.IndexByte(pkg, '/'); i > 0 {
			scope := pkg[:i]
			for _, s := range h.privateScopes {
				if s == scope {
					return true
				}
			}
		}
		return false
	}
	// Unscoped: exact match against the private name list.
	for _, name := range h.privateUnscoped {
		if name == pkg {
			return true
		}
	}
	return false
}

// privateDownServeStale reports whether the handler should serve a stale (local
// cache) copy when the private upstream fails. Never returns true when failClosed
// is set; uses the guard as the canonical source when wired.
func (h *Handler) privateDownServeStale() bool {
	if h.guard != nil {
		return h.guard.ResolvePrivate(verify.OutcomeDown) == verify.ActionServeStale
	}
	return !h.failClosed
}

// selectUpstreams returns the upstream list to use for pkg, applying
// dependency-confusion rules:
//   - Private pkg + private upstream configured → [private]
//   - Private pkg + no private upstream → error (never fall back to public, ever)
//   - Public pkg → h.upstreams (public mirror pool)
//
// A public fallback for private packages is never permitted regardless of the
// failClosed flag: the "no private upstream" window is exactly when an attacker's
// public copy would win (DESIGN-REVIEW §4 H3).
func (h *Handler) selectUpstreams(pkg string) ([]upstream.Upstream, error) {
	if !h.isPrivatePkg(pkg) {
		return h.upstreams, nil
	}
	if h.privateUpstream != nil {
		return []upstream.Upstream{*h.privateUpstream}, nil
	}
	// Private name with no private upstream — always fail (never fall through to public).
	return nil, fmt.Errorf("npm: dep-confusion guard: no private upstream configured for private pkg %q", pkg)
}

// --------------------------------------------------------------------------
// Endpoint implementations (replaces 501 stubs)
// --------------------------------------------------------------------------

// servePackument handles GET/HEAD /<pkg> and GET/HEAD /@<scope>/<pkg>:
// the packument (per-package metadata JSON) is MUTABLE, cached with a short
// TTL and conditional GET revalidation.
func (h *Handler) servePackument(w http.ResponseWriter, r *http.Request, pkg string) {
	ups, err := h.selectUpstreams(pkg)
	if err != nil {
		h.log.Error("npm: dep-confusion guard", "pkg", pkg, "err", err)
		writeError(w, http.StatusBadGateway, "npm: private upstream unavailable")
		return
	}
	ref := packumentRef(pkg)
	h.serveMutable(w, r, ref, contentTypePackument, ups)
}

// serveTarball handles GET/HEAD /<pkg>/-/<file>.tgz: the tarball is IMMUTABLE
// and promoted to the permanent CAS tier via verify-on-write.
func (h *Handler) serveTarball(w http.ResponseWriter, r *http.Request, pkg, file string) {
	ups, err := h.selectUpstreams(pkg)
	if err != nil {
		h.log.Error("npm: dep-confusion guard", "pkg", pkg, "file", file, "err", err)
		writeError(w, http.StatusBadGateway, "npm: private upstream unavailable")
		return
	}
	ref := tarballRef(pkg, file)
	h.serveImmutable(w, r, ref, ups)
}

// --------------------------------------------------------------------------
// Mutable pipeline — packument (ETag revalidation, short TTL, serve-stale)
// --------------------------------------------------------------------------

// serveMutable is the shared pipeline for the mutable packument tier:
//  1. Check the short-TTL cache (fresh entries only; TTL enforced by Lookup).
//  2. On TTL expiry: conditional GET (ETag → 304 serve-stale or 200 re-store).
//  3. On complete miss: fresh fetch → quarantine → store → serve.
//  4. If the upstream is unreachable and stale content exists, serve stale.
func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string, ups []upstream.Upstream) {
	ctx := r.Context()

	// 1. Check fresh mutable cache.
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("npm: mutable lookup", "ref", ref, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// Capture a stale entry for serve-stale-on-upstream-failure if the
	// CacheManager supports it (optional; not all test fakes implement staler).
	var staleEntry *artifact.CacheEntry
	if sm, ok := h.cache.(staler); ok {
		staleEntry, _ = sm.LookupStale(ctx, ref)
	}

	// 2. Upstream required for revalidation or fresh fetch.
	if h.upstreamClt == nil || len(ups) == 0 {
		if staleEntry != nil {
			h.log.Warn("npm: no upstream; serving stale packument", "ref", ref)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Conditional GET revalidation if we have prev ETag/Last-Modified and
	//    a stale body to serve when the upstream answers 304.
	if prevMeta, hasPrev := h.getMutableUpstreamMeta(ctx, ref); hasPrev && staleEntry != nil {
		body, umeta, notModified, revalErr := h.upstreamClt.Revalidate(ctx, ref, prevMeta, ups)
		if revalErr == nil {
			if notModified {
				h.extendMutableTTL(ctx, ref, umeta)
				h.serveFromCache(w, r, ref, staleEntry, ct)
				return
			}
			// 200: new body — store and serve.
			defer body.Close()
			if newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta); storeErr == nil && newEntry != nil {
				h.serveFromCache(w, r, ref, newEntry, ct)
				return
			}
			// Store failed: fall through to a fresh fetch.
		}
	}

	// 4. Fresh fetch.
	body, umeta, fetchErr := h.upstreamClt.Fetch(ctx, ref, ups)
	if fetchErr != nil {
		if h.isPrivatePkg(ref.Name) {
			// Private upstream failed — guard decides action (never public).
			if h.privateDownServeStale() && staleEntry != nil {
				h.log.Warn("npm: private upstream failed; serving stale packument",
					"ref", ref, "err", fetchErr)
				h.serveFromCache(w, r, ref, staleEntry, ct)
				return
			}
			h.log.Error("npm: private upstream failed (fail-closed)", "ref", ref, "err", fetchErr)
			writeError(w, http.StatusServiceUnavailable, "npm: private upstream unavailable")
			return
		}
		// Non-private: serve stale if available, else 502.
		if staleEntry != nil {
			h.log.Warn("npm: upstream failed; serving stale packument", "ref", ref, "err", fetchErr)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		h.log.Error("npm: mutable fetch", "ref", ref, "err", fetchErr)
		writeError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer body.Close()

	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta)
	if storeErr != nil {
		h.log.Error("npm: store packument", "ref", ref, "err", storeErr)
		writeError(w, http.StatusBadGateway, "failed to cache upstream response")
		return
	}
	h.serveFromCache(w, r, ref, newEntry, ct)
}

// --------------------------------------------------------------------------
// Immutable pipeline — tarball (verify-on-write, permanent CAS)
// --------------------------------------------------------------------------

// serveImmutable is the shared pipeline for the immutable tarball tier:
//  1. Try the verified CAS (fast path, no upstream contact).
//  2. On cache miss: fetch → quarantine → verify-on-write (TOFU pin) → promote → serve.
func (h *Handler) serveImmutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ups []upstream.Upstream) {
	ctx := r.Context()

	// 1. Try the verified CAS (cache hit fast path).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("npm: tarball lookup", "ref", ref, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		h.serveTarballFromCache(w, r, ref, entry)
		return
	}

	// 2. Cache miss — upstream required.
	if h.upstreamClt == nil || len(ups) == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Fetch → quarantine → verify-on-write → CAS promotion.
	//    Integrity mismatch (TOFU changed) is surfaced as *cache.VerifyError.
	entry, err = h.fetchAndStoreImmutable(ctx, ref, ups)
	if err != nil {
		var ve *cache.VerifyError
		if errors.As(err, &ve) {
			h.log.Warn("npm: verify-on-write rejected tarball",
				"ref", ref, "tier", ve.Result.Tier, "msg", ve.Result.Message)
			writeError(w, http.StatusBadGateway, "tarball failed integrity verification")
			return
		}
		h.log.Error("npm: fetch tarball", "ref", ref, "err", err)
		writeError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	if entry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	h.serveTarballFromCache(w, r, ref, entry)
}

// --------------------------------------------------------------------------
// Fetch/store helpers
// --------------------------------------------------------------------------

// fetchAndStoreImmutable fetches a tarball from the first healthy upstream,
// streams it through the quarantine/verify-on-write pipeline (TOFU pin on first
// fetch, FAIL on digest change), and promotes the result to the permanent CAS.
//
// Workaround: upstream.buildPath for npm uses (Mutable || Digest == "") to
// select the packument path (ref.Name) vs the tarball path (ref.Name + "/-/" +
// ref.Version). npm tarballs never have a pre-known content digest, so
// ref.Digest is always "" on first fetch — which would send the request to the
// packument endpoint instead of the tarball endpoint. We set a non-empty
// sentinel Digest on the fetch ref only to force the correct tarball URL; the
// actual Store uses the original ref (Digest="") so ChecksumVerifier (which
// only fails when both Digest!="" and digest_mismatch) stays in pass-through
// mode, and TofuVerifier pins on first contact as intended.
func (h *Handler) fetchAndStoreImmutable(ctx context.Context, ref artifact.ArtifactRef, ups []upstream.Upstream) (*artifact.CacheEntry, error) {
	// fetchRef: identical to ref except Digest is non-empty to force the
	// tarball URL path in upstream.buildPath (see doc comment above).
	fetchRef := ref
	fetchRef.Digest = "tarball-fetch" // sentinel; never stored in CAS

	rc, umeta, err := h.upstreamClt.Fetch(ctx, fetchRef, ups)
	if err != nil {
		return nil, fmt.Errorf("upstream fetch: %w", err)
	}
	defer rc.Close()

	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}

	// Store uses the original ref (Digest="") so the verify chain sees the
	// correct ref for TOFU pinning and checksum comparison.
	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		// Preserve *cache.VerifyError so the caller can surface a meaningful 502.
		return nil, storeErr
	}
	// Store removes art.Path on success; cleanup() is a safe no-op.

	return entry, nil
}

// fetchBodyAndStore quarantines an already-opened mutable response body into
// the CAS and writes a TTL-bearing MutableEntry via h.meta (when set).
// The caller is responsible for closing rc after this function returns.
func (h *Handler) fetchBodyAndStore(ctx context.Context, ref artifact.ArtifactRef, rc io.Reader, umeta artifact.UpstreamMeta) (*artifact.CacheEntry, error) {
	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}

	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		return nil, fmt.Errorf("store: %w", storeErr)
	}

	// cache.Store writes the mutable pointer with TTLSeconds=0 (always
	// revalidate). Override it with the configured short TTL so the mutable
	// tier stays fresh for h.mutableTTLSec without unnecessary upstream hits.
	if h.meta != nil {
		me := artifact.MutableEntry{
			Key:          npmMutableKey(ref),
			Protocol:     ref.Protocol,
			Digest:       art.Digest,
			ETag:         umeta.ETag,
			LastModified: umeta.LastModified,
			TTLSeconds:   h.mutableTTLSec,
			Upstream:     umeta.Upstream,
			FetchedAt:    time.Now().UTC(),
		}
		if putErr := h.meta.PutMutable(ctx, me); putErr != nil {
			// Non-fatal: entry is in CAS; TTL tuning is lost for this entry.
			h.log.Warn("npm: write mutable TTL pointer", "ref", ref, "err", putErr)
		}
	}

	return entry, nil
}

// --------------------------------------------------------------------------
// Serve helpers
// --------------------------------------------------------------------------

// serveFromCache writes packument (or other mutable) content to the HTTP response.
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry, ct string) {
	ctx := r.Context()
	rc, cacheEntry, err := h.cache.Serve(ctx, ref, 0, -1)
	if err != nil {
		if errors.Is(err, cache.ErrCacheMiss) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			h.log.Error("npm: serve from cache", "ref", ref, "err", err)
			writeError(w, http.StatusInternalServerError, "cache serve failed")
		}
		return
	}
	if rc == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer rc.Close()

	// Prefer the size from the entry returned by Serve (post-lookup),
	// falling back to the entry supplied by the caller (pre-lookup).
	var size int64
	if cacheEntry != nil && cacheEntry.Size > 0 {
		size = cacheEntry.Size
	} else if entry != nil && entry.Size > 0 {
		size = entry.Size
	}

	w.Header().Set("Content-Type", ct)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, rc)
	}
}

// serveTarballFromCache writes tarball bytes to the HTTP response.
func (h *Handler) serveTarballFromCache(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry) {
	ctx := r.Context()
	rc, cacheEntry, err := h.cache.Serve(ctx, ref, 0, -1)
	if err != nil {
		if errors.Is(err, cache.ErrCacheMiss) {
			writeError(w, http.StatusNotFound, "not found")
		} else {
			h.log.Error("npm: serve tarball from cache", "ref", ref, "err", err)
			writeError(w, http.StatusInternalServerError, "cache serve failed")
		}
		return
	}
	if rc == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	defer rc.Close()

	var size int64
	if cacheEntry != nil && cacheEntry.Size > 0 {
		size = cacheEntry.Size
	} else if entry != nil && entry.Size > 0 {
		size = entry.Size
	}

	w.Header().Set("Content-Type", contentTypeTarball)
	if size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, rc)
	}
}

// --------------------------------------------------------------------------
// Mutable revalidation helpers
// --------------------------------------------------------------------------

// getMutableUpstreamMeta returns the upstream ETag/LastModified from the
// MetadataStore for conditional GET revalidation. Returns (zero, false) when
// h.meta is nil, the entry is absent, or no revalidation state is available.
func (h *Handler) getMutableUpstreamMeta(ctx context.Context, ref artifact.ArtifactRef) (artifact.UpstreamMeta, bool) {
	if h.meta == nil {
		return artifact.UpstreamMeta{}, false
	}
	key := npmMutableKey(ref)
	me, err := h.meta.GetMutable(ctx, key)
	if err != nil || me == nil || (me.ETag == "" && me.LastModified == "") {
		return artifact.UpstreamMeta{}, false
	}
	return artifact.UpstreamMeta{
		ETag:         me.ETag,
		LastModified: me.LastModified,
		Upstream:     me.Upstream,
	}, true
}

// extendMutableTTL updates FetchedAt in the MetadataStore after a 304 Not
// Modified response, renewing the TTL without a new blob transfer.
func (h *Handler) extendMutableTTL(ctx context.Context, ref artifact.ArtifactRef, umeta artifact.UpstreamMeta) {
	if h.meta == nil {
		return
	}
	key := npmMutableKey(ref)
	me, err := h.meta.GetMutable(ctx, key)
	if err != nil || me == nil {
		return
	}
	me.FetchedAt = time.Now().UTC()
	if umeta.ETag != "" {
		me.ETag = umeta.ETag
	}
	if putErr := h.meta.PutMutable(ctx, *me); putErr != nil {
		h.log.Warn("npm: extend mutable TTL", "ref", ref, "err", putErr)
	}
}
