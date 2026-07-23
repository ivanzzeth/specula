package hf

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/cache"
	"github.com/ivanzzeth/specula/internal/coalesce"
	"github.com/ivanzzeth/specula/internal/metrics"
	"github.com/ivanzzeth/specula/internal/upstream"
)

type staler interface {
	LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)
}

type entryServer interface {
	ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error)
}

func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, hubPath string) {
	ref := mutableRef(hubPath)
	ct := contentTypeForPath(hubPath)
	rewriteJSON := strings.HasSuffix(hubPath, ".json") ||
		strings.HasPrefix(hubPath, "api/") || strings.Contains(hubPath, "/api/")
	h.serveMutableCached(w, r, ref, ct, rewriteJSON)
}

func (h *Handler) serveImmutable(w http.ResponseWriter, r *http.Request, hubPath string) {
	// huggingface_hub probes file metadata with HEAD and requires Hub headers
	// (X-Repo-Commit, ETag). Passthrough HEAD so those headers come from upstream
	// without forcing a full CAS write on every probe.
	if r.Method == http.MethodHead {
		h.passthrough(w, r, hubPath)
		return
	}
	ref := immutableRef(hubPath)
	ctx := r.Context()
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		http.Error(w, "cache lookup failed", http.StatusInternalServerError)
		return
	}
	if entry != nil {
		metrics.MarkHit(ctx)
		h.streamEntryWithHubMeta(w, r, entry, hubPath)
		return
	}
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	entry, err = h.coalescedFetch(ctx, ref, func() (*artifact.CacheEntry, error) {
		return h.fetchAndStoreImmutable(ctx, ref)
	})
	if err != nil {
		var ve *cache.VerifyError
		if errors.As(err, &ve) {
			http.Error(w, "verification failed", http.StatusBadGateway)
			return
		}
		h.log.Error("hf: file fetch", "ref", ref, "err", err)
		if upstream.IsNotFound(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	metrics.MarkMiss(ctx)
	h.streamEntryWithHubMeta(w, r, entry, hubPath)
}

func (h *Handler) serveMutableCached(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string, rewriteJSON bool) {
	ctx := r.Context()
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("hf: mutable lookup", "ref", ref, "err", err)
		http.Error(w, "cache lookup failed", http.StatusInternalServerError)
		return
	}
	if entry != nil {
		metrics.MarkHit(ctx)
		h.serveMutableBytes(w, r, ref, entry, ct, rewriteJSON)
		return
	}
	var staleEntry *artifact.CacheEntry
	if sm, ok := h.cache.(staler); ok {
		staleEntry, _ = sm.LookupStale(ctx, ref)
	}
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		if staleEntry != nil {
			metrics.MarkHit(ctx)
			h.serveMutableBytes(w, r, ref, staleEntry, ct, rewriteJSON)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, umeta, fetchErr := h.upstreamClt.Fetch(ctx, ref, h.upstreams)
	if fetchErr != nil {
		if staleEntry != nil {
			metrics.MarkHit(ctx)
			h.log.Warn("hf: upstream failed; serving stale", "ref", ref, "err", fetchErr)
			h.serveMutableBytes(w, r, ref, staleEntry, ct, rewriteJSON)
			return
		}
		h.log.Error("hf: mutable fetch", "ref", ref, "err", fetchErr)
		if upstream.IsNotFound(fetchErr) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer body.Close()
	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta)
	if storeErr != nil {
		h.log.Error("hf: store mutable", "ref", ref, "err", storeErr)
		http.Error(w, "failed to cache upstream response", http.StatusBadGateway)
		return
	}
	metrics.MarkMiss(ctx)
	h.serveMutableBytes(w, r, ref, newEntry, ct, rewriteJSON)
}

func (h *Handler) passthrough(w http.ResponseWriter, r *http.Request, hubPath string) {
	if len(h.upstreams) == 0 {
		http.Error(w, "no upstream configured", http.StatusBadGateway)
		return
	}
	up := firstUpstream(h.upstreams)
	target := strings.TrimRight(up.BaseURL, "/") + "/" + hubPath
	if q := r.URL.RawQuery; q != "" {
		target += "?" + q
	}

	var body io.Reader
	if r.Body != nil {
		body = r.Body
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		h.log.Error("hf: passthrough build request", "err", err, "target", target)
		http.Error(w, "proxy error", http.StatusInternalServerError)
		return
	}
	for k, vs := range r.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}

	client := h.httpClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		h.log.Error("hf: passthrough upstream error", "err", err, "target", target)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, resp.Body)
	}
}

func firstUpstream(ups []upstream.Upstream) upstream.Upstream {
	if len(ups) == 0 {
		return upstream.Upstream{}
	}
	cp := make([]upstream.Upstream, len(ups))
	copy(cp, ups)
	sort.Slice(cp, func(i, j int) bool { return cp[i].Priority < cp[j].Priority })
	return cp[0]
}

func (h *Handler) coalescedFetch(ctx context.Context, ref artifact.ArtifactRef, fn func() (*artifact.CacheEntry, error)) (*artifact.CacheEntry, error) {
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

func (h *Handler) fetchAndStoreImmutable(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error) {
	rc, umeta, err := h.upstreamClt.Fetch(ctx, ref, h.upstreams)
	if err != nil {
		return nil, fmt.Errorf("upstream fetch: %w", err)
	}
	defer rc.Close()
	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}
	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		return nil, storeErr
	}
	return entry, nil
}

func (h *Handler) fetchBodyAndStore(ctx context.Context, ref artifact.ArtifactRef, rc io.Reader, umeta artifact.UpstreamMeta) (*artifact.CacheEntry, error) {
	art, cleanup, err := cache.Quarantine(ctx, h.quarantineDir, rc, umeta)
	if err != nil {
		return nil, fmt.Errorf("quarantine: %w", err)
	}
	entry, storeErr := h.cache.Store(ctx, ref, art)
	if storeErr != nil {
		cleanup()
		return nil, fmt.Errorf("store: %w", err)
	}
	if h.meta != nil {
		me := artifact.MutableEntry{
			Key:          ref.Protocol + ":" + ref.Name + ":" + ref.Version,
			Protocol:     ref.Protocol,
			Digest:       art.Digest,
			ETag:         umeta.ETag,
			LastModified: umeta.LastModified,
			TTLSeconds:   h.mutableTTLSec,
			Upstream:     umeta.Upstream,
			FetchedAt:    time.Now().UTC(),
		}
		if putErr := h.meta.PutMutable(ctx, me); putErr != nil {
			h.log.Warn("hf: write mutable TTL pointer", "ref", ref, "err", putErr)
		}
	}
	return entry, nil
}

func (h *Handler) serveMutableBytes(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry, ct string, rewriteJSON bool) {
	rc, err := h.openEntry(r.Context(), ref, entry)
	if err != nil {
		http.Error(w, "cache read failed", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		http.Error(w, "cache read failed", http.StatusInternalServerError)
		return
	}
	if rewriteJSON {
		data = rewriteJSONURLs(data, requestBase(r), h.pathPrefix)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (h *Handler) streamEntry(w http.ResponseWriter, r *http.Request, entry *artifact.CacheEntry) {
	h.streamEntryWithHubMeta(w, r, entry, "")
}

// streamEntryWithHubMeta serves a cached blob and sets Hub-compatible metadata
// headers (X-Repo-Commit, ETag) so huggingface_hub HEAD probes succeed.
func (h *Handler) streamEntryWithHubMeta(w http.ResponseWriter, r *http.Request, entry *artifact.CacheEntry, hubPath string) {
	rc, err := h.openEntry(r.Context(), artifact.ArtifactRef{}, entry)
	if err != nil {
		http.Error(w, "cache read failed", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", contentTypeForPath(hubPath))
	if entry.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	}
	if etag := entry.ETag; etag != "" {
		w.Header().Set("ETag", etag)
	} else if entry.Digest != "" {
		w.Header().Set("ETag", `"`+strings.TrimPrefix(entry.Digest, "sha256:")+`"`)
	}
	if commit := hubCommitFromPath(hubPath); commit != "" {
		w.Header().Set("X-Repo-Commit", commit)
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc)
	}
}

func hubCommitFromPath(hubPath string) string {
	parts := strings.Split(hubPath, "/")
	for i := 0; i+1 < len(parts); i++ {
		if parts[i] == "resolve" {
			return parts[i+1]
		}
	}
	return ""
}

func (h *Handler) openEntry(ctx context.Context, ref artifact.ArtifactRef, entry *artifact.CacheEntry) (io.ReadCloser, error) {
	if es, ok := h.cache.(entryServer); ok {
		return es.ServeEntry(ctx, entry, 0, -1)
	}
	rc, _, err := h.cache.Serve(ctx, ref, 0, -1)
	return rc, err
}

func contentTypeForPath(hubPath string) string {
	if strings.HasSuffix(hubPath, ".json") ||
		strings.HasPrefix(hubPath, "api/") || strings.Contains(hubPath, "/api/") {
		return "application/json"
	}
	return "application/octet-stream"
}
