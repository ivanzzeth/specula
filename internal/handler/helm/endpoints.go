package helm

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
)

// staler is an optional extension of CacheManager that returns mutable entries
// even when their TTL has expired. Used for serve-stale-on-upstream-failure.
// The production manager (cache.manager) implements this; test fakes may not.
type staler interface {
	LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)
}

// helmMutableKey returns the MetadataStore key for a mutable helm ArtifactRef.
func helmMutableKey(ref artifact.ArtifactRef) string {
	return ref.Protocol + ":" + ref.Name + ":" + ref.Version
}

// contentTypeForFile returns the MIME type for a helm file.
func contentTypeForFile(file string) string {
	switch {
	case file == indexFile:
		return "application/yaml"
	case strings.HasSuffix(file, extChart):
		return "application/octet-stream"
	case strings.HasSuffix(file, extProv):
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// --------------------------------------------------------------------------
// Mutable endpoint — index.yaml
// --------------------------------------------------------------------------

// serveIndex handles GET/HEAD /<repo>/index.yaml (mutable, short TTL).
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request, repo string) {
	ref := indexRef(repo)
	h.serveMutable(w, r, ref, contentTypeForFile(indexFile))
}

// serveMutable is the shared pipeline for the mutable index endpoint:
//
//  1. Check the short-TTL cache (fresh entries only).
//  2. On expiry: attempt a conditional GET (If-None-Match / If-Modified-Since).
//     304 → extend TTL, serve stale.  200 → store new content.
//  3. On complete cache miss: fresh fetch → quarantine → store → serve.
//  4. If the upstream is unreachable and stale content exists, serve stale.
func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string) {
	ctx := r.Context()

	// 1. Check the mutable cache (fresh entries only; TTL enforced by Lookup).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("helm: mutable lookup", "ref", ref, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// Capture a stale entry for serve-stale-on-upstream-failure if the
	// CacheManager supports it.
	var staleEntry *artifact.CacheEntry
	if sm, ok := h.cache.(staler); ok {
		staleEntry, _ = sm.LookupStale(ctx, ref)
	}

	// 2. Upstream required for revalidation or a fresh fetch.
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		if staleEntry != nil {
			h.log.Warn("helm: no upstream; serving stale", "ref", ref)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Conditional GET revalidation if we have prev ETag/Last-Modified.
	if prevMeta, hasPrev := h.getMutableUpstreamMeta(ctx, ref); hasPrev && staleEntry != nil {
		body, umeta, notModified, revalErr := h.upstreamClt.Revalidate(ctx, ref, prevMeta, h.upstreams)
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
			// Fall through to fresh fetch if store failed.
		}
	}

	// 4. Fresh fetch.
	body, umeta, fetchErr := h.upstreamClt.Fetch(ctx, ref, h.upstreams)
	if fetchErr != nil {
		if staleEntry != nil {
			h.log.Warn("helm: upstream failed; serving stale", "ref", ref, "err", fetchErr)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		h.log.Error("helm: mutable fetch", "ref", ref, "err", fetchErr)
		writeError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer body.Close()

	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta)
	if storeErr != nil {
		h.log.Error("helm: store mutable", "ref", ref, "err", storeErr)
		writeError(w, http.StatusBadGateway, "failed to cache upstream response")
		return
	}
	h.serveFromCache(w, r, ref, newEntry, ct)
}

// --------------------------------------------------------------------------
// Immutable endpoint — .tgz and .tgz.prov
// --------------------------------------------------------------------------

// serveChart handles GET/HEAD /<repo>/<chart>.tgz[.prov] (immutable, CAS).
//
// For .tgz requests we also attempt to fetch the matching .prov file and attach
// it as art.Meta.Attachments[0] so the HelmProvVerifier can check the GPG
// signature inside the verify-on-write pipeline (DESIGN-REVIEW §1.1).
//
// For .prov requests the file is fetched and stored as any other immutable
// artifact; no special verification is applied at this stage.
func (h *Handler) serveChart(w http.ResponseWriter, r *http.Request, repo, file string) {
	ref := chartRef(repo, file)
	ct := contentTypeForFile(file)
	ctx := r.Context()

	// 1. Try the verified CAS (cache hit fast path).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("helm: immutable lookup", "repo", repo, "file", file, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// 2. Cache miss — upstream required.
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Fetch → quarantine → verify-on-write → CAS promotion.
	entry, err = h.fetchAndStoreChart(ctx, ref)
	if err != nil {
		var ve *cache.VerifyError
		if errors.As(err, &ve) {
			h.log.Error("helm: verify failed", "repo", repo, "file", file,
				"tier", ve.Result.Tier, "msg", ve.Result.Message)
			writeError(w, http.StatusBadGateway, "artifact verification failed: "+ve.Result.Message)
			return
		}
		h.log.Error("helm: fetch chart", "repo", repo, "file", file, "err", err)
		writeError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	if entry == nil {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	h.serveFromCache(w, r, ref, entry, ct)
}

// fetchAndStoreChart fetches a chart .tgz (or .prov) from the first healthy
// upstream. For .tgz files it additionally tries to fetch the matching .prov
// and attaches its bytes to UpstreamMeta.Attachments[0], so the
// HelmProvVerifier inside the verify chain can check the GPG signature.
func (h *Handler) fetchAndStoreChart(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	rc, umeta, err := h.upstreamClt.Fetch(ctx, ref, h.upstreams)
	if err != nil {
		return nil, fmt.Errorf("upstream fetch: %w", err)
	}
	defer rc.Close()

	// For .tgz files, attempt a best-effort .prov fetch and attach the bytes.
	// A missing or unreadable .prov is not an error — the verifier degrades
	// gracefully (no .prov → TierTofu/TierChecksum instead of TierSigned).
	if strings.HasSuffix(ref.Version, extChart) {
		provRef := chartRef(ref.Name, ref.Version+".prov")
		if provBytes, provErr := h.fetchProvBytes(ctx, provRef); provErr == nil && len(provBytes) > 0 {
			umeta.Attachments = append(umeta.Attachments, provBytes)
		}
	}

	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}

	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		return nil, storeErr // pass through VerifyError unwrapped
	}

	return entry, nil
}

// fetchProvBytes fetches the provenance file for a chart and returns its bytes.
// Returns a nil slice (and non-nil error) when the .prov is absent or the
// upstream cannot be reached. Callers treat this as "no provenance" and
// degrade gracefully rather than failing.
func (h *Handler) fetchProvBytes(ctx context.Context, provRef artifact.ArtifactRef) ([]byte, error) {
	rc, _, err := h.upstreamClt.Fetch(ctx, provRef, h.upstreams)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("helm: read prov: %w", err)
	}
	return b, nil
}

// --------------------------------------------------------------------------
// Shared fetch/store helpers
// --------------------------------------------------------------------------

// fetchBodyAndStore quarantines an already-opened mutable response body
// (index.yaml) into the CAS and writes a TTL-bearing MutableEntry via h.meta
// (when set). The caller is responsible for closing the reader after this
// function returns.
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

	// Override the default always-revalidate TTL with the configured short TTL
	// so the index stays cached for mutableTTLSec without unnecessary upstream hits.
	if h.meta != nil {
		me := artifact.MutableEntry{
			Key:          helmMutableKey(ref),
			Protocol:     ref.Protocol,
			Digest:       art.Digest,
			ETag:         umeta.ETag,
			LastModified: umeta.LastModified,
			TTLSeconds:   h.mutableTTLSec,
			Upstream:     umeta.Upstream,
			FetchedAt:    time.Now().UTC(),
		}
		if putErr := h.meta.PutMutable(ctx, me); putErr != nil {
			// Non-fatal: the index is already in CAS; we lose TTL tuning.
			h.log.Warn("helm: write mutable TTL pointer", "ref", ref, "err", putErr)
		}
	}

	return entry, nil
}

// --------------------------------------------------------------------------
// Shared serve helper
// --------------------------------------------------------------------------

// serveFromCache writes the artifact identified by ref to the HTTP response.
// entry is used to supply Content-Length when known (may be nil if unavailable).
// The actual bytes are read from h.cache.Serve (only verified content is returned).
func (h *Handler) serveFromCache(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry, ct string) {
	ctx := r.Context()
	rc, cacheEntry, err := h.cache.Serve(ctx, ref, 0, -1)
	if err != nil {
		if errors.Is(err, cache.ErrCacheMiss) {
			writeError(w, http.StatusNotFound, "artifact not in cache")
		} else {
			h.log.Error("helm: serve from cache", "ref", ref, "err", err)
			writeError(w, http.StatusInternalServerError, "cache serve failed")
		}
		return
	}
	if rc == nil {
		writeError(w, http.StatusNotFound, "artifact not in cache")
		return
	}
	defer rc.Close()

	// Prefer size from the CacheEntry returned by Serve (post-lookup),
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

// --------------------------------------------------------------------------
// Mutable revalidation helpers
// --------------------------------------------------------------------------

// getMutableUpstreamMeta returns the upstream ETag/LastModified from the
// MetadataStore for conditional GET revalidation. Returns (zero, false) when
// h.meta is nil, the entry is absent, or there is no revalidation state.
func (h *Handler) getMutableUpstreamMeta(ctx context.Context, ref artifact.ArtifactRef) (artifact.UpstreamMeta, bool) {
	if h.meta == nil {
		return artifact.UpstreamMeta{}, false
	}
	key := helmMutableKey(ref)
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

// extendMutableTTL updates the FetchedAt timestamp in the MetadataStore after
// a 304 Not Modified response, renewing the TTL without a new blob transfer.
func (h *Handler) extendMutableTTL(ctx context.Context, ref artifact.ArtifactRef, umeta artifact.UpstreamMeta) {
	if h.meta == nil {
		return
	}
	key := helmMutableKey(ref)
	me, err := h.meta.GetMutable(ctx, key)
	if err != nil || me == nil {
		return
	}
	me.FetchedAt = time.Now().UTC()
	if umeta.ETag != "" {
		me.ETag = umeta.ETag
	}
	if putErr := h.meta.PutMutable(ctx, *me); putErr != nil {
		h.log.Warn("helm: extend mutable TTL", "ref", ref, "err", putErr)
	}
}
