package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/upstream"
)

// serveManifest handles GET/HEAD /v2/<name>/manifests/<reference>.
// reference may be a digest (sha256:…) or a mutable tag.
//
// Flow:
//  1. Resolve digest from mutable-tier cache (for tags).
//  2. Attempt to serve from the verified CAS.
//  3. On cache miss, fetch from upstream (verify-on-write → promote → serve).
func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, imageName, reference string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Step 1: resolve digest from mutable tier (tag→digest map).
	// This returns immediately for digest refs; for tag refs it checks
	// the mutable cache only (no upstream probe here).
	digest, _, err := h.resolveManifestDigest(ctx, imageName, reference)
	if err != nil {
		h.log.Error("oci: resolve manifest digest", "image", imageName, "ref", reference, "err", err)
		writeOCIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to resolve manifest")
		return
	}

	// Step 2: try to serve from the verified CAS.
	if digest != "" {
		if h.tryServeManifestFromCache(w, r, imageName, digest) {
			return
		}
	}

	// Step 3: cache miss — fetch from upstream with verify-on-write.
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}

	newDigest, fetchErr := h.fetchAndStoreManifest(ctx, imageName, reference)
	if fetchErr != nil {
		h.log.Error("oci: fetch manifest from upstream", "image", imageName, "ref", reference, "err", fetchErr)
		writeOCIError(w, http.StatusBadGateway, "MANIFEST_UNKNOWN", "upstream fetch failed")
		return
	}

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
	fetchRef := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  reference,
		Mutable:  true, // buildPath: v2/<name>/manifests/<reference>
	}

	rc, umeta, err := h.upstreamClt.Fetch(ctx, fetchRef, h.upstreams, upstream.WithOCIManifestAccept())
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
