package conda

import (
	"context"
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
)

type staler interface {
	LookupStale(ctx context.Context, ref artifact.ArtifactRef) (*artifact.CacheEntry, error)
}

type entryServer interface {
	ServeEntry(ctx context.Context, entry *artifact.CacheEntry, offset, length int64) (io.ReadCloser, error)
}

func (h *Handler) serveIndex(w http.ResponseWriter, r *http.Request, relPath string) {
	ref := indexRef(relPath)
	h.serveMutable(w, r, ref, contentTypeForPath(relPath))
}

func (h *Handler) servePackage(w http.ResponseWriter, r *http.Request, relPath string) {
	ref := packageRef(relPath)
	h.serveImmutable(w, r, ref)
}

func (h *Handler) serveMutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, ct string) {
	ctx := r.Context()
	entry, err := h.cache.Lookup(ctx, ref)
	if err != nil {
		h.log.Error("conda: mutable lookup", "ref", ref, "err", err)
		http.Error(w, "cache lookup failed", http.StatusInternalServerError)
		return
	}
	if entry != nil {
		metrics.MarkHit(ctx)
		h.serveMutableBytes(w, r, ref, entry, ct)
		return
	}
	var staleEntry *artifact.CacheEntry
	if sm, ok := h.cache.(staler); ok {
		staleEntry, _ = sm.LookupStale(ctx, ref)
	}
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		if staleEntry != nil {
			metrics.MarkHit(ctx)
			h.serveMutableBytes(w, r, ref, staleEntry, ct)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	body, umeta, fetchErr := h.upstreamClt.Fetch(ctx, ref, h.upstreams)
	if fetchErr != nil {
		if staleEntry != nil {
			metrics.MarkHit(ctx)
			h.log.Warn("conda: upstream failed; serving stale index", "ref", ref, "err", fetchErr)
			h.serveMutableBytes(w, r, ref, staleEntry, ct)
			return
		}
		h.log.Error("conda: mutable fetch", "ref", ref, "err", fetchErr)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer body.Close()
	newEntry, storeErr := h.fetchBodyAndStore(ctx, ref, body, umeta)
	if storeErr != nil {
		h.log.Error("conda: store index", "ref", ref, "err", storeErr)
		http.Error(w, "failed to cache upstream response", http.StatusBadGateway)
		return
	}
	metrics.MarkMiss(ctx)
	h.serveMutableBytes(w, r, ref, newEntry, ct)
}

func (h *Handler) serveImmutable(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef) {
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
		h.log.Error("conda: package fetch", "ref", ref, "err", err)
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
			h.log.Warn("conda: write mutable TTL pointer", "ref", ref, "err", putErr)
		}
	}
	return entry, nil
}

func (h *Handler) serveMutableBytes(w http.ResponseWriter, r *http.Request, ref artifact.ArtifactRef, entry *artifact.CacheEntry, ct string) {
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
