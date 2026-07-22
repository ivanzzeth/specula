// Package pypi — endpoint implementations.
//
// serveIndex and serveFile are the real, production-grade endpoint handlers.
// They follow the same two-tier CAS + verify-on-write pipeline as the OCI and
// Go-module handlers (internal/handler/oci, internal/handler/gomod) and mirror
// the mutable / immutable design documented in ARCHITECTURE.md §3 and §8.
package pypi

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
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/upstream"
	"github.com/ivanzzeth/specula/internal/verify"
)

// Content-type constants for PyPI HTTP responses.
const (
	// ctSimpleHTML is the default MIME type for PEP 503 HTML simple indexes.
	ctSimpleHTML = "text/html; charset=utf-8"
	// ctSimpleJSON is the PEP 691 JSON simple index MIME type.
	ctSimpleJSON = "application/vnd.pypi.simple.v1+json"
	// ctWheel is used for wheel / sdist binary file downloads.
	ctWheel = "application/octet-stream"

	// indexVersionJSON is the ArtifactRef.Version sentinel for a PEP 691 JSON
	// simple index, distinct from indexVersion ("simple") so the two formats
	// occupy separate mutable-tier cache slots.
	indexVersionJSON = "simple-json"
)

// staler is an optional CacheManager extension for serve-stale-on-upstream-failure
// (DESIGN-REVIEW H1 / gomod handler pattern).
type staler interface {
	LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)
}

// entryServer is an optional extension of CacheManager that serves the bytes of
// a CacheEntry the caller ALREADY holds, with no re-lookup and no freshness
// gate. This is what makes serve-stale-on-upstream-failure work: cache.Serve
// re-runs Lookup, which reports a miss for a stale mutable entry, so a stale
// entry can only be rendered through ServeEntry (DESIGN-REVIEW fix H1).
// The production manager (cache.manager) implements this; test fakes may not.
type entryServer interface {
	ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error)
}

// wantsJSON reports whether the client signals a preference for the PEP 691
// JSON simple index via its Accept header.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), ctSimpleJSON)
}

// indexRefForRequest builds the mutable ArtifactRef for a /simple/<project>/
// request, scoped by the requested content format (HTML vs JSON) so the two
// formats have independent mutable-tier cache entries.
func indexRefForRequest(project string, r *http.Request) artifact.ArtifactRef {
	version := indexVersion // "simple" for HTML (from pypi.go)
	if wantsJSON(r) {
		version = indexVersionJSON
	}
	return artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     project,
		Version:  version,
		Mutable:  true,
	}
}

// contentTypeForRequest returns the HTTP Content-Type for the simple index
// format the client requested.
func contentTypeForRequest(r *http.Request) string {
	if wantsJSON(r) {
		return ctSimpleJSON
	}
	return ctSimpleHTML
}

// pypiMutableKey returns the MetadataStore key for a mutable PyPI ArtifactRef.
// Mirrors cache.mutableKey (unexported): protocol + ":" + name + ":" + version.
func pypiMutableKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

// --------------------------------------------------------------------------
// isPrivate / selectUpstreams — dependency-confusion guard (DESIGN-REVIEW §4)
// --------------------------------------------------------------------------

// isPrivate reports whether the PEP 503-normalised project name is org-owned
// and must resolve ONLY from the private upstream (DESIGN-REVIEW §4). The
// dependency-confusion guard is the authoritative source when wired; the inline
// manifest walk is a backward-compat fallback for handlers built without private
// names (e.g. pure public-mirror deployments).
func (h *Handler) isPrivate(project string) bool {
	if h.guard != nil {
		return h.guard.IsPrivate(project)
	}
	norm := normalizeProject(project)
	for _, n := range h.privateNames {
		if normalizeProject(n) == norm {
			return true
		}
	}
	return false
}

// privateDownServeStale reports whether the handler should serve a stale (local
// cache) copy when the private upstream fails for a private name. It is never
// true when no stale entry is available; it is always false when FailClosed=true.
// The guard is the canonical source; inline failClosed is used as a fallback.
func (h *Handler) privateDownServeStale() bool {
	if h.guard != nil {
		return h.guard.ResolvePrivate(verify.OutcomeDown) == verify.ActionServeStale
	}
	return !h.failClosed
}

// selectUpstreams returns the upstream list for the given project name.
//
//   - Private name + private upstream configured → single-item private list.
//   - Private name + no private upstream → fail-closed error (never public).
//   - Public name → h.upstreams (public mirror list).
func (h *Handler) selectUpstreams(project string) ([]upstream.Upstream, error) {
	if !h.isPrivate(project) {
		return h.upstreams, nil
	}
	if h.privateUpstream == nil {
		return nil, fmt.Errorf("private name %q: no private upstream configured (fail-closed)", project)
	}
	return []upstream.Upstream{*h.privateUpstream}, nil
}

// --------------------------------------------------------------------------
// serveIndex — GET/HEAD /simple/<project>/ (mutable, short TTL)
// --------------------------------------------------------------------------

// serveIndex handles GET/HEAD /simple/<project>/ (PEP 503 HTML + PEP 691 JSON).
// The index page is mutable (new releases appear over time) and is cached in
// the short-TTL mutable tier with conditional-GET revalidation.
//
// # PEP 691 content negotiation
//
// When the client sends Accept: application/vnd.pypi.simple.v1+json (PEP 691)
// we first probe the "simple-json" cache slot: if a JSON response was
// previously cached (e.g. from a JSON-capable upstream), we serve it directly
// with the JSON Content-Type.
//
// If the JSON slot is empty we fall back to the HTML path.  This is valid
// per PEP 691 §4.1 ("A server MAY respond with any version they support")
// — pip ≥21 always includes "text/html" in its Accept fallback and will
// re-parse the response as HTML when the server returns text/html.
//
// Full JSON negotiation (forwarding the Accept header to the upstream so that
// mirrors which support PEP 691 return JSON) requires a new
// upstream.WithAcceptHeader(string) RequestOption that the upstream package
// currently does not expose.  See KNOWN-LIMITATIONS below.
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request, project string) {
	ups, err := h.selectUpstreams(project)
	if err != nil {
		// Private name with no private upstream configured — always fail regardless
		// of failClosed. A public fallback here is the confusion attack vector:
		// the window when the private upstream is misconfigured is exactly when an
		// attacker's public copy would win (DESIGN-REVIEW §4 H3).
		h.log.Warn("pypi: private name — no private upstream configured (fail-closed)",
			"project", project)
		writeError(w, http.StatusServiceUnavailable,
			"private package source not available")
		return
	}

	// PEP 691: if the client prefers JSON and a JSON copy is already cached,
	// serve it.  Otherwise fall through to the HTML path (valid graceful
	// degradation per PEP 691 §4.1).
	if wantsJSON(r) {
		jsonRef := artifact.ArtifactRef{
			Protocol: Protocol,
			Name:     project,
			Version:  indexVersionJSON,
			Mutable:  true,
		}
		jsonEntry, lookErr := h.cache.Lookup(r.Context(), jsonRef)
		if lookErr != nil {
			h.log.Error("pypi: json cache lookup", "ref", jsonRef, "err", lookErr)
			// fall through to HTML
		} else if jsonEntry != nil {
			// PEP 691 JSON slot hit: body comes from cache.
			metrics.MarkHit(r.Context())
			h.serveFromCache(w, r, jsonRef, jsonEntry, ctSimpleJSON)
			return
		}
		// JSON slot is empty — fall through to HTML with HTML Content-Type.
		// pip accepts this per PEP 691 (servers may respond with any supported
		// format; pip's Accept list always includes text/html as a fallback).
		h.log.Debug("pypi: JSON not cached; falling back to HTML for JSON client",
			"project", project)
	}

	ref := indexRef(project)
	h.serveMutable(w, r, ref, ctSimpleHTML, ups)
}

// serveMutable is the shared pipeline for the mutable /simple/ index endpoint:
//
//  1. Check the short-TTL cache (fresh → serve immediately).
//  2. If stale: attempt conditional GET (ETag / Last-Modified). 304 → extend TTL.
//  3. Complete miss or 200: fresh fetch → quarantine → verify-on-write → store → serve.
//  4. If upstream down and stale content exists → serve stale (H1 fix).
func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string, ups []upstream.Upstream) {
	ctx := r.Context()

	// 1. Fresh cache lookup (TTL-gated).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("pypi: mutable lookup", "ref", ref, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		// Fresh or soft-expired (SWR) cache hit: body from cache, no blocking upstream.
		metrics.MarkHit(ctx)
		if entry.SoftExpired {
			h.swrRefreshAsync(ref, ups)
		}
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// Capture stale entry for serve-stale-on-upstream-failure fallback
	// (requires the production CacheManager that implements staler).
	var staleEntry *artifact.CacheEntry
	if sm, ok := h.cache.(staler); ok {
		staleEntry, _ = sm.LookupStale(ctx, ref)
	}

	// 2. Upstream required for revalidation or fresh fetch.
	if h.upstreamClt == nil || len(ups) == 0 {
		if staleEntry != nil {
			// Serve-stale with no upstream at all: the body came from cache.
			metrics.MarkHit(ctx)
			h.log.Warn("pypi: no upstream configured; serving stale", "ref", ref)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Conditional GET revalidation when stale entry is available.
	if prevMeta, hasPrev := h.getMutableUpstreamMeta(ctx, ref); hasPrev && staleEntry != nil {
		body, umeta, notModified, revalErr := h.upstreamClt.Revalidate(ctx, ref, prevMeta, ups)
		if revalErr == nil {
			if notModified {
				// 304: the upstream sent no body; the bytes we serve came from
				// cache. A hit under the bytes-origin definition.
				metrics.MarkHit(ctx)
				h.extendMutableTTL(ctx, ref, umeta)
				h.serveFromCache(w, r, ref, staleEntry, ct)
				return
			}
			// 200: store the new body and serve it.
			defer body.Close()
			if newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta); storeErr == nil && newEntry != nil {
				// Revalidation returned a NEW body from the upstream.
				metrics.MarkMiss(ctx)
				h.serveFromCache(w, r, ref, newEntry, ct)
				return
			}
			// Store failed: fall through to a fresh full fetch.
		}
	}

	// 4. Fresh fetch.
	body, umeta, fetchErr := h.upstreamClt.Fetch(ctx, ref, ups)
	if fetchErr != nil {
		if h.isPrivate(ref.Name) {
			// Private upstream failed — guard decides action (never public).
			// ActionServeStale: serve from local cache only if a stale copy exists.
			// ActionFailClosed (or no stale): 5xx.
			if h.privateDownServeStale() && staleEntry != nil {
				// Serve-stale on upstream failure (H1): body came from cache.
				metrics.MarkHit(ctx)
				h.log.Warn("pypi: private upstream failed; serving stale",
					"project", ref.Name, "err", fetchErr)
				h.serveFromCache(w, r, ref, staleEntry, ct)
				return
			}
			h.log.Error("pypi: private upstream failed (fail-closed)",
				"project", ref.Name, "err", fetchErr)
			writeError(w, http.StatusServiceUnavailable, "private upstream unavailable")
			return
		}
		// Non-private: serve stale if available, else 502.
		if staleEntry != nil {
			// Serve-stale on upstream failure (H1): body came from cache.
			metrics.MarkHit(ctx)
			h.log.Warn("pypi: upstream failed; serving stale", "ref", ref, "err", fetchErr)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		h.log.Error("pypi: mutable fetch", "ref", ref, "err", fetchErr)
		writeError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer body.Close()

	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta)
	if storeErr != nil {
		h.log.Error("pypi: store mutable index", "ref", ref, "err", storeErr)
		writeError(w, http.StatusBadGateway, "failed to cache upstream response")
		return
	}
	// Cache miss: the body was fetched from an upstream and stored.
	metrics.MarkMiss(ctx)
	h.serveFromCache(w, r, ref, newEntry, ct)
}

// --------------------------------------------------------------------------
// serveFile — GET/HEAD /packages/<path>/<file> (immutable, CAS, TOFU)
// --------------------------------------------------------------------------

// serveFile handles GET/HEAD /packages/<path>/<file>. Wheel and sdist files
// are immutable (a released file never changes) and are promoted to the
// permanent CAS tier. TOFU pins the sha256 computed during streaming on first
// fetch and fails-closed if the same version is later re-fetched with different
// content (DESIGN-REVIEW §5).
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, name, file string) {
	// Dependency-confusion guard: parse the project name from the filename and
	// route private names exclusively to the private upstream (DESIGN-REVIEW §4).
	project := extractProjectFromFile(file)
	ups := h.upstreams
	if project != "" && h.isPrivate(project) {
		if h.privateUpstream == nil {
			// Private name with no private upstream — always fail. A public
			// fallback here is the dependency-confusion attack path (H3).
			h.log.Warn("pypi: private name — no private upstream (fail-closed)",
				"project", project)
			writeError(w, http.StatusServiceUnavailable,
				"private package source not available")
			return
		}
		ups = []upstream.Upstream{*h.privateUpstream}
	}

	ref := fileRef(name, file)
	h.serveImmutable(w, r, ref, ups)
}

// serveImmutable is the shared pipeline for immutable wheel/sdist files:
//
//  1. Look up the verified CAS entry (fast path — no upstream contact).
//  2. Cache miss: fetch from upstream → quarantine → verify-on-write → CAS → serve.
func (h *Handler) serveImmutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ups []upstream.Upstream) {
	ctx := r.Context()

	// 1. Verified CAS lookup (fast path).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("pypi: immutable lookup", "ref", ref, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		// CAS hit: the body comes from cache, no upstream body transfer.
		metrics.MarkHit(ctx)
		h.serveFromCache(w, r, ref, entry, ctWheel)
		return
	}

	// 2. Cache miss — upstream required.
	if h.upstreamClt == nil || len(ups) == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Fetch → quarantine → verify-on-write → CAS.
	entry, err = h.coalescedFetch(ctx, ref, func() (*artifact.CacheEntry, error) {
		return h.fetchAndStoreFile(ctx, ref, ups)
	})
	if err != nil {
		h.log.Error("pypi: fetch immutable", "ref", ref, "err", err)
		writeError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	if entry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// Cache miss: the file body was fetched from an upstream and promoted to CAS.
	metrics.MarkMiss(ctx)
	h.serveFromCache(w, r, ref, entry, ctWheel)
}

// --------------------------------------------------------------------------
// Shared fetch / store helpers
// --------------------------------------------------------------------------

// fetchBodyAndStore quarantines an already-opened mutable response body,
// stores it via the verify-on-write pipeline, and overrides the mutable-tier
// TTL pointer with h.mutableTTLSec (mirroring the gomod handler pattern).
func (h *Handler) fetchBodyAndStore(ctx context.Context, ref artifact.ArtifactRef, rc io.Reader, umeta artifact.UpstreamMeta) (*artifact.CacheEntry, error) {
	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}

	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup() // no-op if Store already removed art.Path
		return nil, fmt.Errorf("store: %w", storeErr)
	}
	// cache.Store removes art.Path on success; cleanup() is safe no-op.

	// Override the mutable-tier TTL written by cache.Store (default = 0 = always
	// revalidate) with the configured short TTL so repeated requests within
	// h.mutableTTLSec do not hit the upstream.
	if h.meta != nil {
		me := artifact.MutableEntry{
			Key:          pypiMutableKey(ref),
			Protocol:     ref.Protocol,
			Digest:       art.Digest,
			ETag:         umeta.ETag,
			LastModified: umeta.LastModified,
			TTLSeconds:   h.mutableTTLSec,
			Upstream:     umeta.Upstream,
			FetchedAt:    time.Now().UTC(),
		}
		if putErr := h.meta.PutMutable(ctx, me); putErr != nil {
			h.log.Warn("pypi: write mutable TTL pointer", "ref", ref, "err", putErr)
		}
	}

	return entry, nil
}

// fetchAndStoreFile fetches an immutable wheel/sdist from upstream, streams it
// through the quarantine / verify-on-write pipeline, and promotes it to CAS.
//
// # ref.Digest and upstream URL routing
//
// PyPI file refs have ref.Digest="" before the content is fetched (the digest
// is not known from the download URL alone). upstream.buildPath for "pypi" uses
// the condition (ref.Mutable || ref.Digest == "") to distinguish the mutable
// simple-index path ("simple/<name>/") from the immutable package path
// ("packages/<name>/<version>"):
//
//	if ref.Mutable || ref.Digest == "" → "simple/<name>/"   ← WRONG for file fetch
//	else                               → "packages/<name>/<version>"
//
// To get the correct "packages/" path we create a temporary copy of ref with
// Digest="pending" (any non-empty sentinel) for the Fetch call. The real store
// call uses the original ref (Digest="") so:
//
//   - ChecksumVerifier: ref.Digest=="" → no reference check → StatusPass.
//   - TofuVerifier: pins art.Digest on first sight; detects tampering on re-fetch.
func (h *Handler) fetchAndStoreFile(ctx context.Context, ref artifact.ArtifactRef, ups []upstream.Upstream) (*artifact.CacheEntry, error) {
	// Build a fetch ref: Digest != "" so buildPath uses "packages/<name>/<version>".
	fetchRef := ref
	fetchRef.Digest = "pending" // sentinel; never stored or validated

	rc, umeta, err := h.upstreamClt.Fetch(ctx, fetchRef, ups)
	if err != nil {
		return nil, fmt.Errorf("upstream fetch: %w", err)
	}
	defer rc.Close()

	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}

	// Store with original ref (Digest=""):
	//  - ChecksumVerifier PASS (no reference digest to compare).
	//  - TofuVerifier pins art.Digest on first fetch (StatusWarn = first-lock);
	//    on re-fetch with a different digest → StatusFail (TOFU mismatch).
	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		return nil, fmt.Errorf("store: %w", storeErr)
	}
	return entry, nil
}

// --------------------------------------------------------------------------
// Serve helper
// --------------------------------------------------------------------------

// serveFromCache writes the artifact identified by ref to the HTTP response.
// Only already-verified CAS content is ever served (fix C2).
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry, ct string) {
	ctx := r.Context()
	rc, cacheEntry, err := h.serveBytes(ctx, ref, entry)
	if err != nil {
		if errors.Is(err, cache.ErrCacheMiss) {
			writeError(w, http.StatusNotFound, "artifact not in cache")
		} else {
			h.log.Error("pypi: serve from cache", "ref", ref, "err", err)
			writeError(w, http.StatusInternalServerError, "cache serve failed")
		}
		return
	}
	if rc == nil {
		writeError(w, http.StatusNotFound, "artifact not in cache")
		return
	}
	defer rc.Close()

	// Prefer size from the CacheEntry returned by Serve; fall back to the one
	// supplied by the caller (which may have been obtained earlier via Lookup).
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

// --------------------------------------------------------------------------
// Mutable revalidation helpers (mirror gomod handler pattern)
// --------------------------------------------------------------------------

// swrRefreshAsync kicks a coalesced background revalidation for an XFetch
// soft-expired hit (RFC 5861 stale-while-revalidate).
func (h *Handler) swrRefreshAsync(ref artifact.ArtifactRef, ups []upstream.Upstream) {
	if h.upstreamClt == nil || len(ups) == 0 {
		return
	}
	key := coalesce.FetchKey(ref.Protocol, ref.Name, ref.Version, ref.Digest) + "|swr"
	cache.StartBackgroundRefresh(key, func(ctx context.Context) error {
		if prevMeta, hasPrev := h.getMutableUpstreamMeta(ctx, ref); hasPrev {
			body, umeta, notModified, err := h.upstreamClt.Revalidate(ctx, ref, prevMeta, ups)
			if err != nil {
				return err
			}
			if notModified {
				h.extendMutableTTL(ctx, ref, umeta)
				return nil
			}
			defer body.Close()
			_, err = h.fetchBodyAndStore(ctx, ref, body, umeta)
			return err
		}
		body, umeta, err := h.upstreamClt.Fetch(ctx, ref, ups)
		if err != nil {
			return err
		}
		defer body.Close()
		_, err = h.fetchBodyAndStore(ctx, ref, body, umeta)
		return err
	})
}

// getMutableUpstreamMeta returns ETag/LastModified for a mutable entry from
// the MetadataStore (used for conditional-GET revalidation). Returns (zero,
// false) when h.meta is nil, the entry is absent, or there is no revalidation
// state.
func (h *Handler) getMutableUpstreamMeta(ctx context.Context, ref artifact.ArtifactRef) (artifact.UpstreamMeta, bool) {
	if h.meta == nil {
		return artifact.UpstreamMeta{}, false
	}
	key := pypiMutableKey(ref)
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

// extendMutableTTL refreshes FetchedAt in the MetadataStore after a 304
// Not Modified response, renewing the TTL without a byte transfer.
func (h *Handler) extendMutableTTL(ctx context.Context, ref artifact.ArtifactRef, umeta artifact.UpstreamMeta) {
	if h.meta == nil {
		return
	}
	key := pypiMutableKey(ref)
	me, err := h.meta.GetMutable(ctx, key)
	if err != nil || me == nil {
		return
	}
	me.FetchedAt = time.Now().UTC()
	if umeta.ETag != "" {
		me.ETag = umeta.ETag
	}
	if putErr := h.meta.PutMutable(ctx, *me); putErr != nil {
		h.log.Warn("pypi: extend mutable TTL", "ref", ref, "err", putErr)
	}
}

// --------------------------------------------------------------------------
// Dependency-confusion helper
// --------------------------------------------------------------------------

// extractProjectFromFile parses the distribution name from a wheel or sdist
// filename using the PEP 427 / PEP 625 naming conventions: the project name
// is the first '-'-delimited component of the basename (before the version).
// The result is PEP 503-normalised so it can be compared to h.privateNames.
//
// Examples:
//
//	"numpy-1.21.0-cp39-cp39-linux_x86_64.whl" → "numpy"
//	"Flask-2.3.0.tar.gz"                       → "flask"
//	"Django-4.0-py3-none-any.whl"              → "django"
//	"my_lib-0.1.0.whl"                         → "my-lib"
//
// Returns "" when the filename does not follow the convention (no '-').
func extractProjectFromFile(file string) string {
	// Strip recognised archive extensions (longest suffix wins).
	base := file
	for _, ext := range []string{
		".whl", ".tar.gz", ".tar.bz2", ".tar.xz", ".zip", ".egg",
	} {
		if strings.HasSuffix(base, ext) {
			base = base[:len(base)-len(ext)]
			break
		}
	}
	idx := strings.IndexByte(base, '-')
	if idx <= 0 {
		return ""
	}
	return normalizeProject(base[:idx])
}

// serveBytes returns a reader for the artifact's bytes plus the entry they
// belong to. When the caller already holds an entry — including a STALE one
// obtained from LookupStale — the bytes are served directly from it via
// ServeEntry. Re-resolving through cache.Serve would re-run the freshness gate
// and return ErrCacheMiss for stale mutable content, 404-ing the very content
// the serve-stale path just decided to serve (DESIGN-REVIEW fix H1).
func (h *Handler) serveBytes(ctx context.Context, ref artifact.ArtifactRef, entry *artifact.CacheEntry) (io.ReadCloser, *artifact.CacheEntry, error) {
	if es, ok := h.cache.(entryServer); ok && entry != nil {
		rc, err := es.ServeEntry(ctx, entry, 0, -1)
		return rc, entry, err
	}
	return h.cache.Serve(ctx, ref, 0, -1)
}

// coalescedFetch runs fn under the cold-fetch single-flight so concurrent
// callers for the SAME request identity share one upstream round trip
// (ARCHITECTURE §7). See coalesce.FetchLocked for cross-replica semantics.
func (h *Handler) coalescedFetch(
	ctx context.Context,
	ref artifact.ArtifactRef,
	fn func() (*artifact.CacheEntry, error),
) (*artifact.CacheEntry, error) {
	key := coalesce.FetchKey(ref.Protocol, ref.Name, ref.Version, ref.Digest)
	return coalesce.FetchLocked(ctx, h.fetchSF, h.locker, key, 0,
		func(ctx context.Context) (*artifact.CacheEntry, bool, error) {
			e, err := h.cache.Lookup(ctx, ref)
			if err != nil {
				return nil, false, err
			}
			if e != nil {
				return e, true, nil
			}
			return nil, false, nil
		},
		fn,
	)
}
