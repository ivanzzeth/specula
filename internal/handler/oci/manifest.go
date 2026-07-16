package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/ivanzzeth/specula/internal/artifact"
)

// serveManifest handles GET/HEAD /v2/<name>/manifests/<reference>.
// reference may be a digest (sha256:…) or a mutable tag.
func (h *Handler) serveManifest(w http.ResponseWriter, r *http.Request, imageName, reference string) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	digest, ok, err := h.resolveManifestDigest(ctx, imageName, reference)
	if err != nil {
		h.log.Error("oci: resolve manifest digest", "image", imageName, "ref", reference, "err", err)
		writeOCIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to resolve manifest")
		return
	}
	if !ok {
		writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
		return
	}

	ref := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  digest,
		Digest:   digest,
		Mutable:  false,
	}

	rc, entry, err := h.cache.Serve(ctx, ref, 0, -1)
	if err != nil || rc == nil {
		writeOCIError(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest not in cache")
		return
	}
	defer rc.Close()

	// Manifests are JSON and always small (< a few hundred KB); buffer to
	// detect media type and set an accurate Content-Type header.
	data, err := io.ReadAll(rc)
	if err != nil {
		writeOCIError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to read manifest")
		return
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
}

// resolveManifestDigest returns the immutable digest for the given reference.
// For digest references it returns immediately; for tags it checks the mutable
// tier (MetadataStore or CacheManager.Lookup) and optionally revalidates via
// an upstream HEAD probe.
func (h *Handler) resolveManifestDigest(ctx context.Context, imageName, reference string) (digest string, found bool, err error) {
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
		// Stale or absent: fall through to upstream revalidation.
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

	// 3. Fetch from upstream (HEAD probe first to avoid consuming rate limits).
	if h.upstreamClt != nil && len(h.upstreams) > 0 {
		d, err := h.fetchManifestDigestFromUpstream(ctx, imageName, reference)
		if err != nil {
			return "", false, fmt.Errorf("upstream fetch: %w", err)
		}
		if d != "" {
			return d, true, nil
		}
	}

	return "", false, nil
}

// fetchManifestDigestFromUpstream performs an upstream HEAD probe (no body = no
// rate-limit burn) then fetches the manifest if the digest changed or is new.
// On success it stores the manifest in the CAS via CacheManager.Store.
func (h *Handler) fetchManifestDigestFromUpstream(ctx context.Context, imageName, tag string) (string, error) {
	fetchRef := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  tag,
		Mutable:  true,
	}

	// Revalidate uses conditional GET / HEAD; notModified=true means the cached
	// digest is still current (we only have a cached digest at this point, not
	// the full mutable entry, so pass empty prev meta).
	rc, upMeta, notModified, err := h.upstreamClt.Revalidate(ctx, fetchRef, artifact.UpstreamMeta{}, h.upstreams)
	if err != nil {
		return "", fmt.Errorf("upstream revalidate: %w", err)
	}
	if notModified || rc == nil {
		return "", nil
	}
	defer rc.Close()

	// Read manifest bytes to compute digest and store in CAS.
	data, err := io.ReadAll(rc)
	if err != nil {
		return "", fmt.Errorf("reading upstream manifest: %w", err)
	}

	art := &artifact.Artifact{
		Digest: upMeta.Upstream,
		Size:   int64(len(data)),
		Meta:   upMeta,
	}

	immutableRef := artifact.ArtifactRef{
		Protocol: "oci",
		Name:     imageName,
		Version:  tag,
		Upstream: upMeta.Upstream,
	}

	entry, err := h.cache.Store(ctx, immutableRef, art)
	if err != nil {
		return "", fmt.Errorf("cache store: %w", err)
	}
	if entry == nil {
		return "", nil
	}
	return entry.Digest, nil
}

// detectManifestMediaType inspects the JSON body to extract the mediaType field.
// Falls back to Docker v2 schema 2 if not present.
func detectManifestMediaType(data []byte) string {
	var m struct {
		MediaType string `json:"mediaType"`
	}
	if json.Unmarshal(data, &m) == nil && m.MediaType != "" {
		return m.MediaType
	}
	return "application/vnd.docker.distribution.manifest.v2+json"
}
