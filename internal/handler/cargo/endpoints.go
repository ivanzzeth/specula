package cargo

import (
	"context"
	"encoding/json"
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
)

type staler interface {
	LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)
}

type entryServer interface {
	ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error)
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request, indexPath string) {
	ref := indexRef(indexPath)
	h.serveMutable(w, r, ref, "application/json", h.upstreams, indexPath == "config.json")
}

func (h *Handler) serveCrate(w http.ResponseWriter, r *http.Request, name, version string) {
	ref := crateRef(name, version)
	h.serveImmutable(w, r, ref, h.dlUpstreams)
}

func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string, ups []upstream.Upstream, rewriteConfig bool) {
	ctx := r.Context()
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("cargo: mutable lookup", "ref", ref, "err", err)
		http.Error(w, "cache lookup failed", http.StatusInternalServerError)
		return
	}
	if entry != nil {
		metrics.MarkHit(ctx)
		h.serveMutableBytes(w, r, ref, entry, ct, rewriteConfig)
		return
	}
	var staleEntry *artifact.CacheEntry
	if sm, ok := h.cache.(staler); ok {
		staleEntry, _ = sm.LookupStale(ctx, ref)
	}
	if h.upstreamClt == nil || len(ups) == 0 {
		if staleEntry != nil {
			metrics.MarkHit(ctx)
			h.serveMutableBytes(w, r, ref, staleEntry, ct, rewriteConfig)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, umeta, fetchErr := h.upstreamClt.Fetch(ctx, ref, ups)
	if fetchErr != nil {
		if staleEntry != nil {
			metrics.MarkHit(ctx)
			h.log.Warn("cargo: upstream failed; serving stale index", "ref", ref, "err", fetchErr)
			h.serveMutableBytes(w, r, ref, staleEntry, ct, rewriteConfig)
			return
		}
		h.log.Error("cargo: mutable fetch", "ref", ref, "err", fetchErr)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer body.Close()
	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta)
	if storeErr != nil {
		h.log.Error("cargo: store index", "ref", ref, "err", storeErr)
		http.Error(w, "failed to cache upstream response", http.StatusBadGateway)
		return
	}
	metrics.MarkMiss(ctx)
	h.serveMutableBytes(w, r, ref, newEntry, ct, rewriteConfig)
}

func (h *Handler) serveImmutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ups []upstream.Upstream) {
	ctx := r.Context()
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		http.Error(w, "cache lookup failed", http.StatusInternalServerError)
		return
	}
	if entry != nil {
		metrics.MarkHit(ctx)
		h.streamEntry(w, r, entry)
		return
	}
	if h.upstreamClt == nil || len(ups) == 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	entry, err = h.coalescedFetch(ctx, ref, func() (*artifact.CacheEntry, error) {
		return h.fetchAndStoreImmutable(ctx, ref, ups)
	})
	if err != nil {
		var ve *cache.VerifyError
		if errors.As(err, &ve) {
			http.Error(w, "verification failed", http.StatusBadGateway)
			return
		}
		h.log.Error("cargo: crate fetch", "ref", ref, "err", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	metrics.MarkMiss(ctx)
	h.streamEntry(w, r, entry)
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

func (h *Handler) fetchAndStoreImmutable(ctx context.Context, ref artifact.ArtifactRef, ups []upstream.Upstream) (*artifact.CacheEntry, error) {
	rc, umeta, err := h.upstreamClt.Fetch(ctx, ref, ups)
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
		return nil, fmt.Errorf("store: %w", storeErr)
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
			h.log.Warn("cargo: write mutable TTL pointer", "ref", ref, "err", putErr)
		}
	}
	return entry, nil
}

func (h *Handler) serveMutableBytes(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry, ct string, rewriteConfig bool) {
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
	if rewriteConfig {
		data = rewriteConfigJSON(data, requestBase(r), h.pathPrefix)
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (h *Handler) streamEntry(w http.ResponseWriter, r *http.Request, entry *artifact.CacheEntry) {
	rc, err := h.openEntry(r.Context(), artifact.ArtifactRef{}, entry)
	if err != nil {
		http.Error(w, "cache read failed", http.StatusInternalServerError)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	if entry.Size > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(entry.Size, 10))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, rc)
	}
}

func (h *Handler) openEntry(ctx context.Context, ref artifact.ArtifactRef, entry *artifact.CacheEntry) (io.ReadCloser, error) {
	if es, ok := h.cache.(entryServer); ok {
		return es.ServeEntry(ctx, entry, 0, -1)
	}
	rc, _, err := h.cache.Serve(ctx, ref, 0, -1)
	return rc, err
}

func requestBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		scheme = fp
	}
	host := r.Host
	if host == "" {
		host = r.URL.Host
	}
	return scheme + "://" + host
}

func rewriteConfigJSON(data []byte, base, prefix string) []byte {
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return data
	}
	p := strings.TrimRight(prefix, "/")
	doc["dl"] = base + p + "/crates"
	doc["api"] = base + p
	out, err := json.Marshal(doc)
	if err != nil {
		return data
	}
	return out
}
