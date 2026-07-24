package pypi

import (
	"context"
	"io"
	"strings"

	"github.com/ivanzzeth/specula/internal/artifact"
	"github.com/ivanzzeth/specula/internal/publishmeta"
	"github.com/ivanzzeth/specula/internal/upstream"
)

const maxMetaBody = 8 << 20 // 8 MiB

// enrichPublishTime sets umeta.PublishedAt from PEP 691 upload-time or
// Warehouse /pypi/<project>/json upload_time_iso_8601. Best-effort; never
// fails the file download.
func (h *Handler) enrichPublishTime(ctx context.Context, ref artifact.ArtifactRef, umeta *artifact.UpstreamMeta) {
	if umeta == nil || !umeta.PublishedAt.IsZero() {
		return
	}
	filename := strings.TrimSpace(ref.Version)
	if filename == "" {
		return
	}
	// file refs use Name = hash path prefix (ab/cd); project comes from filename.
	project := extractProjectFromFile(filename)
	if project == "" {
		return
	}

	if body, ok := h.simpleJSONBody(ctx, project); ok {
		if t, ok := publishmeta.FromPyPISimpleJSON(body, filename); ok {
			umeta.PublishedAt = t
			return
		}
	}
	if body, ok := h.warehouseJSONBody(ctx, project); ok {
		if t, ok := publishmeta.FromPyPIWarehouseJSON(body, filename); ok {
			umeta.PublishedAt = t
		}
	}
}

func (h *Handler) simpleJSONBody(ctx context.Context, project string) ([]byte, bool) {
	jsonRef := artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     project,
		Version:  indexVersionJSON,
		Mutable:  true,
	}
	if entry, err := h.cache.Lookup(ctx, jsonRef); err == nil && entry != nil {
		if body, ok := h.readCachedBody(ctx, entry); ok {
			return body, true
		}
	}
	ups, err := h.selectUpstreams(project)
	if err != nil || h.upstreamClt == nil || len(ups) == 0 {
		return nil, false
	}
	rc, meta, fetchErr := h.upstreamClt.Fetch(ctx, jsonRef, ups, upstream.WithAcceptHeader(ctSimpleJSON))
	if fetchErr != nil || rc == nil {
		return nil, false
	}
	defer rc.Close()
	if !strings.Contains(strings.ToLower(meta.ContentType), "json") {
		// Upstream ignored Accept and returned HTML — do not treat as PEP 691.
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, maxMetaBody))
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(rc, maxMetaBody+1))
	if err != nil || len(body) == 0 || len(body) > maxMetaBody {
		return nil, false
	}
	return body, true
}

func (h *Handler) warehouseJSONBody(ctx context.Context, project string) ([]byte, bool) {
	ups, err := h.selectUpstreams(project)
	if err != nil || h.upstreamClt == nil || len(ups) == 0 {
		return nil, false
	}
	wref := artifact.ArtifactRef{
		Protocol: Protocol,
		Name:     project,
		Version:  "json",
		Mutable:  true,
	}
	rc, _, fetchErr := h.upstreamClt.Fetch(ctx, wref, ups)
	if fetchErr != nil || rc == nil {
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
