package oci

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// serveManifest handles GET/HEAD /v2/<name>/manifests/<reference>.
// reference may be a digest (sha256:…) or a mutable tag.
//
// Flow (hosted repo):
//  1. isHosted check — if hosted, enforce visibility before any CAS access.
//  2. Resolve digest from mutable tier; attempt to serve from CAS.
//  3. CAS miss → 404 (hosted repos are authoritative; never fetch upstream).
//
// Flow (non-hosted / pull-through):
//  1. isHosted returns false; skip auth gate.
//  2. Resolve digest from mutable tier; attempt to serve from CAS.
//  3. CAS miss → fetch from upstream (verify-on-write → promote → serve).
func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, imageName, reference string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Hosted-first gate: resolve hosted status before any CAS lookup so that
	// visibility is enforced for both cache-hit and cache-miss paths. The gate
	// is inert (isHosted → false) until a HostedResolver is wired, preserving
	// existing pull-through behaviour byte-for-byte.
	hostedRepo := h.isHosted(ctx, imageName)
	if hostedRepo {
		if err := h.checkHostedRead(ctx, imageName); err != nil {
			if errors.Is(err, ErrUnauthorized) {
				writeOCIError(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
				return
			}
			writeOCIError(w, http.StatusForbidden, "DENIED", "insufficient access")
			return
		}
	}

	// Step 1: resolve digest from mutable tier (tag→digest map).
	// Returns immediately for digest refs; for tag refs checks the mutable cache
	// only (no upstream probe here).
	digest, _, err := h.resolveManifestDigest(ctx, imageName, reference)
	if err != nil {
		h.log.Error("oci: resolve manifest digest", "image", imageName, "ref", reference, "err", err)
		writeOCIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to resolve manifest")
		return
	}

	// Step 2: try to serve from the verified CAS.
	if digest != "" {
		if h.tryServeManifestFromCache(w, r, imageName, digest) {
			// CAS hit: the body comes from cache, no upstream body transfer.
			// Marked here rather than inside tryServeManifestFromCache, which is
			// also the renderer for the post-fetch (miss) path below.
			metrics.MarkHit(ctx)
			return
		}
		// Meta hit but blob missing (M1): the tag resolved to a digest but the
		// bytes are not in the CAS, so they must be refetched → a miss, marked
		// on the fetch path below.
	}

	// Cache miss path.
	if hostedRepo {
		// Hosted repos are authoritative local content: a CAS miss means the
		// manifest does not exist here. Never fall through to upstream.
		writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}

	// Owned-namespace names are authoritative-local even before the repo row
	// exists: a manifest miss is a definitive 404, never an upstream leak. This
	// keeps push and pull of org namespaces correct when an OCI pull-through
	// upstream is also configured.
	if h.isOwnedNamespace(ctx, imageName) {
		writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}

	// Non-hosted: Step 3 — fetch from upstream with verify-on-write.
	ups, _, ok := h.upstreamForName(imageName)
	if h.upstreamClt == nil || !ok || len(ups) == 0 {
		writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}

	newDigest, fetchErr := coalesce.FetchLocked(ctx, h.fetchSF, h.locker,
		coalesce.FetchKey("oci", "manifest:"+imageName, reference, ""),
		0,
		func(ctx context.Context) (string, bool, error) {
			d, _, rerr := h.resolveManifestDigest(ctx, imageName, reference)
			if rerr != nil {
				return "", false, rerr
			}
			if d == "" {
				return "", false, nil
			}
			okRef := artifact.ArtifactRef{Protocol: "oci", Name: imageName, Digest: d}
			e, lerr := h.cache.Lookup(ctx, okRef)
			if lerr != nil {
				return "", false, lerr
			}
			if e != nil {
				return d, true, nil
			}
			return "", false, nil
		},
		func() (string, error) {
			return h.fetchAndStoreManifest(ctx, imageName, reference)
		})
	if fetchErr != nil {
		h.log.Error("oci: fetch manifest from upstream", "image", imageName, "ref", reference, "err", fetchErr)
		if upstream.IsNotFound(fetchErr) {
			writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		writeOCIError(w, http.StatusBadGateway, "MANIFEST_UNKNOWN", "upstream fetch failed")
		return
	}

	// Cache miss: the manifest body was fetched from an upstream and promoted.
	// This also covers the meta-hit-but-blob-missing refetch (M1), where the
	// Step 2 tryServe above returned false and marked nothing.
	metrics.MarkMiss(ctx)

	if !h.tryServeManifestFromCache(w, r, imageName, newDigest) {
		writeOCIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "manifest stored but not serveable")
	}
}

// tryServeManifestFromCache attempts to read the manifest for digest from the
// verified CAS and write a complete HTTP response. Returns true when the response
// was written (success or HEAD). Returns false on any cache miss or error so the
// caller can attempt upstream fetch.
func (h *Handler) tryServeManifestFromCache(w http.ResponseWriter, r *http.Request, imageName, digest string) bool {
	ctx := r.Context()
	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  digest,
		Digest:   digest,
		Mutable:  false,
	}

	rc, entry, err := h.cache.Serve(ctx, ref, 0, -1)
	if err != nil || rc == nil {
		return false
	}
	defer rc.Close()

	// Manifests are JSON and always small (< a few hundred KB); buffer to
	// detect media type and set an accurate Content-Type header.
	data, err := io.ReadAll(rc)
	if err != nil {
		return false
	}

	size := int64(len(data))
	if entry != nil && entry.Size > 0 {
		size = entry.Size
	}

	ct := detectManifestMediaType(data)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Docker-Content-Digest", digest)
	w.Header().Set("Docker-Distribution-Api-Version", "registry/2.0")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	w.WriteHeader(http.StatusOK)

	if r.Method == http.MethodGet {
		_, _ = w.Write(data)
	}
	return true
}

// resolveManifestDigest returns the immutable digest for the given reference
// from the mutable-tier cache only (no upstream probe). For digest references
// it returns immediately. Returns ("", false, nil) when not found in cache.
func (h *Handler) resolveManifestDigest(ctx context.Context, imageName, reference string) (digest string, found bool, err error) {
	// Digest refs are self-describing; no lookup needed.
	if isDigestRef(reference) {
		return reference, true, nil
	}

	// 1. Direct mutable-tier lookup via MetadataStore (production path).
	if h.meta != nil {
		key := mutableKey(imageName, reference)
		entry, err := h.meta.GetMutable(ctx, key)
		if err != nil {
			return "", false, fmt.Errorf("meta GetMutable: %w", err)
		}
		if entry != nil && entry.Digest != "" && !isMutableExpired(entry) {
			return entry.Digest, true, nil
		}
	}

	// 2. CacheManager.Lookup for mutable ref (fake CacheManager handles this in tests).
	mutableRef := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  reference,
		Mutable:  true,
	}
	cacheEntry, err := h.cache.Lookup(ctx, mutableRef)
	if err != nil {
		return "", false, fmt.Errorf("cache Lookup: %w", err)
	}
	if cacheEntry != nil && cacheEntry.Digest != "" {
		return cacheEntry.Digest, true, nil
	}

	return "", false, nil
}

// fetchAndStoreManifest fetches the manifest for imageName/reference from the
// first healthy upstream, streams it through the quarantine/verify-on-write
// pipeline, and promotes it to the CAS.
//
// The reference may be a tag or a content digest. On success the returned
// digest is the sha256 of the promoted bytes (verified by streaming hash).
//
// The OCI manifest Accept header is always sent so registries return the
// correct content type for multi-arch image indexes.
func (h *Handler) fetchAndStoreManifest(ctx context.Context, imageName, reference string) (string, error) {
	ups, fetchName, ok := h.upstreamForName(imageName)
	if !ok || len(ups) == 0 {
		return "", fmt.Errorf("no upstream for %q", imageName)
	}
	fetchRef := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     fetchName, // stripped of allowlisted registry host for upstream path
		Version:  reference,
		Mutable:  true, // buildPath: v2/<name>/manifests/<reference>
	}

	rc, umeta, err := h.upstreamClt.Fetch(ctx, fetchRef, ups, upstream.WithOCIManifestAccept())
	if err != nil {
		return "", fmt.Errorf("upstream fetch manifest: %w", err)
	}
	defer rc.Close()

	// Stream to quarantine file; compute real sha256 digest (verify-on-write).
	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return "", fmt.Errorf("quarantine manifest: %w", err)
	}

	// Store in CAS keyed by the REAL digest computed from the bytes.
	// Mutable=false, Version=digest so meta.Get keys on (oci, name, sha256:…).
	immutableRef := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  art.Digest,
		Digest:   art.Digest,
		Mutable:  false,
	}
	_, storeErr := h.cache.Store(ctx, immutableRef, art)
	if storeErr != nil {
		// cleanup is safe even if Store already removed art.Path on verify fail.
		cleanup()
		return "", fmt.Errorf("cache store manifest: %w", storeErr)
	}
	// Store removes art.Path on success; cleanup() is a safe no-op.

	// Write the mutable tag→digest pointer for fast TTL-based lookup next time.
	// Only possible when h.meta is injected AND the reference is a tag (not digest).
	if h.meta != nil && !isDigestRef(reference) {
		me := artifact.MutableEntry{
			Key:          mutableKey(imageName, reference),
			Protocol:     "oci",
			Digest:       art.Digest,
			ETag:         umeta.ETag,
			LastModified: umeta.LastModified,
			TTLSeconds:   h.mutableTTLSec,
			Upstream:     umeta.Upstream,
			FetchedAt:    time.Now().UTC(),
		}
		if putErr := h.meta.PutMutable(ctx, me); putErr != nil {
			// Non-fatal: manifest is in CAS; we just lose fast tag revalidation.
			h.log.Warn("oci: write mutable manifest pointer", "image", imageName, "ref", reference, "err", putErr)
		}
	}

	return art.Digest, nil
}

// detectManifestMediaType inspects the JSON body to extract the mediaType field.
// Falls back to heuristic detection for OCI images that omit the field, and
// finally to Docker v2 schema 2 as a conservative default.
func detectManifestMediaType(data []byte) string {
	var m struct {
		MediaType string `json:"mediaType"`
		// OCI image index has a "manifests" array; OCI image manifest has "layers".
		Manifests json.RawMessage `json:"manifests"`
		Layers    json.RawMessage `json:"layers"`
	}
	if json.Unmarshal(data, &m) == nil {
		if m.MediaType != "" {
			return m.MediaType
		}
		// OCI formats may omit mediaType; distinguish index vs single-arch by
		// presence of the "manifests" vs "layers" top-level key.
		if len(m.Manifests) > 0 {
			return "application/vnd.oci.image.index.v1+json"
		}
		if len(m.Layers) > 0 {
			return "application/vnd.oci.image.manifest.v1+json"
		}
	}
	return "application/vnd.docker.distribution.manifest.v2+json"
}
