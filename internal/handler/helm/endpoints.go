package helm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/metrics"
)

// staler is an optional extension of CacheManager that returns mutable entries
// even when their TTL has expired. Used for serve-stale-on-upstream-failure.
// The production manager (cache.manager) implements this; test fakes may not.
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
//
// The fetched index.yaml is transparently rewritten so that any absolute chart
// download URLs point to just the chart filename (a relative URL). This ensures
// that when helm resolves chart download URLs it uses the Specula proxy URL as
// the base and all chart fetches flow through the cache.
//
// Helm Chart Repository Spec (https://helm.sh/docs/topics/chart_repository/):
//
//	"urls: A list of URLs for each version of the chart. Relative URLs are
//	 resolved against the repository URL."
func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request, repo string) {
	ref := indexRef(repo)
	h.serveMutable(w, r, ref, contentTypeForFile(indexFile), rewriteIndexURLs)
}

// serveMutable is the shared pipeline for the mutable index endpoint:
//
//  1. Check the short-TTL cache (fresh entries only).
//  2. On expiry: attempt a conditional GET (If-None-Match / If-Modified-Since).
//     304 → extend TTL, serve stale.  200 → store new content.
//  3. On complete cache miss: fresh fetch → quarantine → store → serve.
//  4. If the upstream is unreachable and stale content exists, serve stale.
//
// transform, when non-nil, is applied to the upstream body bytes before they
// are quarantined and stored. Used by serveIndex to rewrite chart URLs.
func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string, transform func([]byte) ([]byte, error)) {
	ctx := r.Context()

	// 1. Check the mutable cache (fresh entries only; TTL enforced by Lookup).
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("helm: mutable lookup", "ref", ref, "err", err)
		writeError(w, http.StatusInternalServerError, "cache lookup failed")
		return
	}
	if entry != nil {
		// Fresh or soft-expired (SWR) cache hit: body from cache, no blocking upstream.
		metrics.MarkHit(ctx)
		if entry.SoftExpired {
			h.swrRefreshAsync(ref, transform)
		}
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
			// Serve-stale with no upstream at all: the body came from cache.
			metrics.MarkHit(ctx)
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
				// 304: the upstream sent no body; the bytes we serve came from
				// cache. A hit under the bytes-origin definition.
				metrics.MarkHit(ctx)
				h.extendMutableTTL(ctx, ref, umeta)
				h.serveFromCache(w, r, ref, staleEntry, ct)
				return
			}
			// 200: new body — store and serve.
			defer body.Close()
			if newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta, transform); storeErr == nil && newEntry != nil {
				// Revalidation returned a NEW body from the upstream.
				metrics.MarkMiss(ctx)
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
			// Serve-stale on upstream failure (H1): body came from cache.
			metrics.MarkHit(ctx)
			h.log.Warn("helm: upstream failed; serving stale", "ref", ref, "err", fetchErr)
			h.serveFromCache(w, r, ref, staleEntry, ct)
			return
		}
		h.log.Error("helm: mutable fetch", "ref", ref, "err", fetchErr)
		writeError(w, http.StatusBadGateway, "upstream fetch failed")
		return
	}
	defer body.Close()

	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta, transform)
	if storeErr != nil {
		h.log.Error("helm: store mutable", "ref", ref, "err", storeErr)
		writeError(w, http.StatusBadGateway, "failed to cache upstream response")
		return
	}
	// Cache miss: the body was fetched from an upstream and stored.
	metrics.MarkMiss(ctx)
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
		// CAS hit: the body comes from cache, no upstream body transfer.
		metrics.MarkHit(ctx)
		h.serveFromCache(w, r, ref, entry, ct)
		return
	}

	// 2. Cache miss — upstream required.
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	// 3. Fetch → quarantine → verify-on-write → CAS promotion.
	entry, err = h.coalescedFetch(ctx, ref, func() (*artifact.CacheEntry, error) {
		return h.fetchAndStoreChart(ctx, ref)
	})
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

	// Cache miss: the chart body was fetched from an upstream and promoted to CAS.
	metrics.MarkMiss(ctx)
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
//
// transform, when non-nil, is applied to the buffered body bytes before
// quarantine. This allows content to be rewritten (e.g., URL rewriting in
// index.yaml) before it enters the CAS. index.yaml files are small enough
// that buffering in memory is safe.
func (h *Handler) fetchBodyAndStore(ctx context.Context, ref artifact.ArtifactRef, rc io.Reader, umeta artifact.UpstreamMeta, transform func([]byte) ([]byte, error)) (*artifact.CacheEntry, error) {
	var reader io.Reader = rc
	if transform != nil {
		data, err := io.ReadAll(rc)
		if err != nil {
			return nil, fmt.Errorf("read body for transform: %w", err)
		}
		transformed, tErr := transform(data)
		if tErr != nil {
			// Log and fall back to the original — degrade gracefully rather
			// than blocking chart discovery on a transform error.
			h.log.Warn("helm: index URL rewrite failed; storing original", "ref", ref, "err", tErr)
			reader = bytes.NewReader(data)
		} else {
			reader = bytes.NewReader(transformed)
		}
	}
	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, reader, umeta)
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
	rc, cacheEntry, err := h.serveBytes(ctx, ref, entry)
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

// swrRefreshAsync kicks a coalesced background revalidation for an XFetch
// soft-expired hit (RFC 5861 stale-while-revalidate).
func (h *Handler) swrRefreshAsync(ref artifact.ArtifactRef, transform func([]byte) ([]byte, error)) {
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		return
	}
	key := coalesce.FetchKey(ref.Protocol, ref.Name, ref.Version, ref.Digest) + "|swr"
	cache.StartBackgroundRefresh(key, func(ctx context.Context) error {
		if prevMeta, hasPrev := h.getMutableUpstreamMeta(ctx, ref); hasPrev {
			body, umeta, notModified, err := h.upstreamClt.Revalidate(ctx, ref, prevMeta, h.upstreams)
			if err != nil {
				return err
			}
			if notModified {
				h.extendMutableTTL(ctx, ref, umeta)
				return nil
			}
			defer body.Close()
			_, err = h.fetchBodyAndStore(ctx, ref, body, umeta, transform)
			return err
		}
		body, umeta, err := h.upstreamClt.Fetch(ctx, ref, h.upstreams)
		if err != nil {
			return err
		}
		defer body.Close()
		_, err = h.fetchBodyAndStore(ctx, ref, body, umeta, transform)
		return err
	})
}

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

// --------------------------------------------------------------------------
// index.yaml URL rewriting
// --------------------------------------------------------------------------

// rewriteIndexURLs rewrites absolute http/https chart download URLs in a Helm
// index.yaml to relative filenames (the last URL path segment). This ensures
// that when helm resolves a chart URL it uses the Specula proxy URL as the
// base, so all chart downloads flow through the proxy's cache.
//
// Helm Chart Repository Spec (https://helm.sh/docs/topics/chart_repository/):
//
//	"urls: A list of URLs for each version of the chart. Relative URLs are
//	 resolved against the repository URL."
//
// An absolute URL like https://upstream/charts/mysql-1.6.9.tgz becomes the
// relative filename mysql-1.6.9.tgz. helm then resolves that against the repo
// URL (e.g. http://specula:5104/helm/charts) to get the final download URL
// http://specula:5104/helm/charts/mysql-1.6.9.tgz.
//
// If the YAML cannot be parsed the original bytes are returned unchanged so
// Specula degrades gracefully rather than blocking chart discovery.
func rewriteIndexURLs(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return data, nil // degrade gracefully
	}
	rewriteYAMLURLs(doc.Content[0])
	out, err := yaml.Marshal(doc.Content[0])
	if err != nil {
		return data, nil // degrade gracefully
	}
	return out, nil
}

// rewriteYAMLURLs recursively walks a parsed yaml.Node tree and replaces
// every absolute http/https URL found inside "urls" sequence nodes with just
// the filename (the last slash-delimited segment of the URL path). Non-http/s
// values and relative URLs are left unchanged.
func rewriteYAMLURLs(n *yaml.Node) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			keyNode := n.Content[i]
			valNode := n.Content[i+1]
			if keyNode.Value == "urls" && valNode.Kind == yaml.SequenceNode {
				// Rewrite each URL entry in this sequence.
				for _, item := range valNode.Content {
					if item.Kind != yaml.ScalarNode {
						continue
					}
					u := item.Value
					if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
						continue // already relative or non-http — leave as-is
					}
					if idx := strings.LastIndexByte(u, '/'); idx >= 0 && idx < len(u)-1 {
						item.Value = u[idx+1:]
					}
				}
			} else {
				// Recurse into mapping values (not into "urls" key nodes).
				rewriteYAMLURLs(valNode)
			}
		}
	case yaml.SequenceNode:
		for _, child := range n.Content {
			rewriteYAMLURLs(child)
		}
	}
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
