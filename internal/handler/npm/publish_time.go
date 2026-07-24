package npm

import (
	"context"
	"io"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/publishmeta"
)

const maxMetaBody = 8 << 20 // 8 MiB — packuments are metadata JSON

// enrichPublishTime sets umeta.PublishedAt from the registry packument's
// time[version]. Prefers a cached packument; on miss, best-effort fetches
// one from upstream (does not fail the tarball download on metadata errors).
func (h *Handler) enrichPublishTime(ctx context.Context, ref artifact.ArtifactRef, umeta *artifact.UpstreamMeta) {
	if umeta == nil || !umeta.PublishedAt.IsZero() {
		return
	}
	ver := publishmeta.VersionFromNPMTarball(ref.Name, ref.Version)
	if ver == "" {
		return
	}
	body, ok := h.packumentBody(ctx, ref.Name)
	if !ok {
		return
	}
	if t, ok := publishmeta.FromNPMPackument(body, ver); ok {
		umeta.PublishedAt = t
	}
}

func (h *Handler) packumentBody(ctx context.Context, pkg string) ([]byte, bool) {
	pref := packumentRef(pkg)
	if entry, err := h.cache.Lookup(ctx, pref); err == nil && entry != nil {
		if body, ok := h.readCachedBody(ctx, entry); ok {
			return body, true
		}
	}
	if h.upstreamClt == nil || len(h.upstreams) == 0 {
		return nil, false
	}
	rc, _, err := h.upstreamClt.Fetch(ctx, pref, h.upstreams)
	if err != nil || rc == nil {
		return nil, false
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxMetaBody+1))
	if err != nil || len(body) == 0 || len(body) > maxMetaBody {
		return nil, false
	}
	return body, true
}

func (h *Handler) readCachedBody(ctx context.Context, entry *artifact.CacheEntry) ([]byte, bool) {
	es, ok := h.cache.(entryServer)
	if !ok || entry == nil {
		return nil, false
	}
	rc, err := es.ServeEntry(ctx, entry, 0, -1)
	if err != nil || rc == nil {
		return nil, false
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, maxMetaBody+1))
	if err != nil || len(body) > maxMetaBody {
		return nil, false
	}
	return body, true
}
